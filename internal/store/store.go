// Package store persists reaparr's user-configurable settings, connections
// and keeper records to a SQLite database under a data directory. State is
// held in memory and every Update writes the whole of it back in one
// transaction; the file is 0600 since it holds API keys. The database also
// holds when each title was last unkept (see RecordUnkept).
//
// Earlier versions kept the same state in dataDir/config.json. The first
// Open against a database with no state yet imports that file, then renames
// it to config.json.migrated so it's kept but never read again.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary still builds with CGO_ENABLED=0
)

// Settings holds the user-configurable tunables that aren't connection
// details — see internal/settings for the env-override convention layered
// on top of these.
type Settings struct {
	LogLevel          string `json:"log_level"`
	PollSchedule      string `json:"poll_schedule"`
	MoviesGracePeriod string `json:"movies_grace_period"` // e.g. "7d" — see settings.ParseGracePeriod (min 1 day)
	TVGracePeriod     string `json:"tv_grace_period"`
	// DaemonEnabled controls the scheduled sweep. When false, nothing is
	// deleted automatically — the dashboard still shows what's due, and
	// deletions happen only via its buttons.
	DaemonEnabled bool `json:"daemon_enabled"`
	// RemoveEndedSeries makes a season delete also remove the series from
	// Sonarr when it has ended and no season has an episode file left. Off by
	// default: it deletes a Sonarr entry nobody is watching.
	RemoveEndedSeries bool `json:"remove_ended_series"`
	// KeepTag is the Radarr/Sonarr tag that marks a movie or series as a
	// keeper: reaparr never deletes anything carrying it.
	KeepTag string `json:"keep_tag"`
	// JellyfinUsers limits whose watched state counts: a comma-separated
	// list of Jellyfin usernames or user IDs. Empty means every user.
	JellyfinUsers string `json:"jellyfin_users"`
}

// DefaultSettings mirrors config.go's own defaults, so a fresh install's
// persisted store (before any env var or UI edit) matches what the
// stateless cron daemon has always defaulted to.
func DefaultSettings() Settings {
	return Settings{
		LogLevel:          "info",
		PollSchedule:      "@hourly",
		MoviesGracePeriod: "7d",
		TVGracePeriod:     "7d",
		DaemonEnabled:     true,
		KeepTag:           "reaparr-keep",
	}
}

// Connection holds one service's URL + API key.
type Connection struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

// Connections holds every external service reaparr can talk to, as entered
// in the dashboard — env-var values are never written here (see
// settings.ResolveConnections). The sweep requires Jellyfin, Radarr or
// Sonarr, and Seerr (see sweeper.missingServices).
type Connections struct {
	Jellyfin Connection `json:"jellyfin"`
	Radarr   Connection `json:"radarr"`
	Sonarr   Connection `json:"sonarr"`
	Seerr    Connection `json:"seerr"`
}

// DefaultConnections mirrors config.go's own default base URLs.
func DefaultConnections() Connections {
	return Connections{
		Jellyfin: Connection{URL: "http://jellyfin:8096"},
		Radarr:   Connection{URL: "http://radarr:7878"},
		Sonarr:   Connection{URL: "http://sonarr:8989"},
		Seerr:    Connection{URL: "http://seerr:5055"},
	}
}

// byService names each connection as it's stored in the connections table.
func (c *Connections) byService() map[string]*Connection {
	return map[string]*Connection{
		"jellyfin": &c.Jellyfin,
		"radarr":   &c.Radarr,
		"sonarr":   &c.Sonarr,
		"seerr":    &c.Seerr,
	}
}

// Keeper is who put a keep tag on a title through the dashboard. The tag
// itself lives in Radarr/Sonarr and is all the sweep looks at; this only says
// who may take it off again (see internal/api).
type Keeper struct {
	Sub  string `json:"sub"`  // the sign-in's stable subject, what ownership is decided on
	Name string `json:"name"` // for display to admins; never used to decide anything
}

// State is the full persisted state.
type State struct {
	Settings    Settings    `json:"settings"`
	Connections Connections `json:"connections"`
	// Keepers maps "radarr:123" / "sonarr:45" to whoever kept that title. A
	// kept title with no entry (kept before this was recorded, or by hand in
	// Radarr/Sonarr) has no owner: only an admin may unkeep it.
	Keepers map[string]Keeper `json:"keepers,omitempty"`
}

const (
	dbFile     = "reaparr.db"
	legacyFile = "config.json"
)

// migrations[i] takes the schema from version i to i+1 (PRAGMA
// user_version). Append only: a released step is never edited.
var migrations = []string{
	`CREATE TABLE settings (
		id                  INTEGER PRIMARY KEY CHECK (id = 1),
		log_level           TEXT    NOT NULL,
		poll_schedule       TEXT    NOT NULL,
		movies_grace_period TEXT    NOT NULL,
		tv_grace_period     TEXT    NOT NULL,
		daemon_enabled      INTEGER NOT NULL,
		remove_ended_series INTEGER NOT NULL,
		keep_tag            TEXT    NOT NULL,
		jellyfin_users      TEXT    NOT NULL
	);
	CREATE TABLE connections (
		service TEXT PRIMARY KEY,
		url     TEXT NOT NULL,
		api_key TEXT NOT NULL
	);
	CREATE TABLE keepers (
		ref  TEXT PRIMARY KEY,
		sub  TEXT NOT NULL,
		name TEXT NOT NULL
	);`,
	`CREATE TABLE unkept (
		ref       TEXT PRIMARY KEY,
		unkept_at TEXT NOT NULL
	);`,
}

// Store guards State with a mutex and persists it to dataDir/reaparr.db.
type Store struct {
	mu      sync.RWMutex
	db      *sql.DB
	dataDir string
	state   State
}

// Open loads state from dataDir/reaparr.db, creating the directory and the
// database if they don't exist yet. A database with no state in it is filled
// from dataDir/config.json if there is one, else with defaults.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	path := filepath.Join(dataDir, dbFile)
	// Create the file ourselves so it's 0600 from the start; SQLite gives
	// its -wal and -shm files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creating database file: %w", err)
	}
	f.Close()

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// One connection: writes are rare and small, and it rules out
	// SQLITE_BUSY between this process's own connections.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dataDir: dataDir}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	if err := migrate(s.db); err != nil {
		return err
	}
	found, err := s.load()
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	s.state = State{Settings: DefaultSettings(), Connections: DefaultConnections()}
	legacy := filepath.Join(s.dataDir, legacyFile)
	raw, err := os.ReadFile(legacy)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s.saveLocked()
	case err != nil:
		return fmt.Errorf("reading %s: %w", legacyFile, err)
	}
	// Unmarshal over the defaults, as the JSON store always did, so a field
	// missing from an older file keeps its default.
	if err := json.Unmarshal(raw, &s.state); err != nil {
		return fmt.Errorf("parsing %s: %w", legacyFile, err)
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	// Only once the import is committed: a failure before this leaves the
	// database without state, so the next start imports again.
	if err := os.Rename(legacy, legacy+".migrated"); err != nil {
		return fmt.Errorf("renaming %s after importing it: %w", legacyFile, err)
	}
	return nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this reaparr understands (%d)", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating schema to version %d: %w", v+1, err)
		}
		// PRAGMA takes no bound parameters.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrating schema to version %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrating schema to version %d: %w", v+1, err)
		}
	}
	return nil
}

// load reads state from the database into s.state. found=false means the
// database holds no state yet.
func (s *Store) load() (found bool, err error) {
	st := State{Connections: DefaultConnections()}
	err = s.db.QueryRow(`SELECT log_level, poll_schedule, movies_grace_period, tv_grace_period,
		daemon_enabled, remove_ended_series, keep_tag, jellyfin_users FROM settings WHERE id = 1`).Scan(
		&st.Settings.LogLevel, &st.Settings.PollSchedule, &st.Settings.MoviesGracePeriod, &st.Settings.TVGracePeriod,
		&st.Settings.DaemonEnabled, &st.Settings.RemoveEndedSeries, &st.Settings.KeepTag, &st.Settings.JellyfinUsers)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading settings: %w", err)
	}

	rows, err := s.db.Query(`SELECT service, url, api_key FROM connections`)
	if err != nil {
		return false, fmt.Errorf("reading connections: %w", err)
	}
	defer rows.Close()
	conns := st.Connections.byService()
	for rows.Next() {
		var service string
		var c Connection
		if err := rows.Scan(&service, &c.URL, &c.APIKey); err != nil {
			return false, fmt.Errorf("reading connections: %w", err)
		}
		if dst, ok := conns[service]; ok {
			*dst = c
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading connections: %w", err)
	}

	krows, err := s.db.Query(`SELECT ref, sub, name FROM keepers`)
	if err != nil {
		return false, fmt.Errorf("reading keepers: %w", err)
	}
	defer krows.Close()
	for krows.Next() {
		var ref string
		var k Keeper
		if err := krows.Scan(&ref, &k.Sub, &k.Name); err != nil {
			return false, fmt.Errorf("reading keepers: %w", err)
		}
		if st.Keepers == nil {
			st.Keepers = map[string]Keeper{}
		}
		st.Keepers[ref] = k
	}
	if err := krows.Err(); err != nil {
		return false, fmt.Errorf("reading keepers: %w", err)
	}

	s.state = st
	return true, nil
}

// DataDir returns the directory state is persisted under.
func (s *Store) DataDir() string {
	return s.dataDir
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// View runs fn with a read lock held over a copy of the current state.
func (s *Store) View(fn func(State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.state)
}

// Update runs fn with a write lock held, letting it mutate state in place,
// then persists the result to disk.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	return s.saveLocked()
}

// RecordUnkept stores at as when each title in refs ("radarr:123" /
// "sonarr:45", like the keepers) was last unkept through the dashboard,
// replacing any earlier time. The sweep starts a title's grace period no
// earlier than this. These live outside State: they're never edited, only
// added to.
func (s *Store) RecordUnkept(refs []string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("saving unkeep times: %w", err)
	}
	defer tx.Rollback() // a no-op once committed

	for _, ref := range refs {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO unkept (ref, unkept_at) VALUES (?, ?)`,
			ref, at.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("saving unkeep time of %s: %w", ref, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saving unkeep times: %w", err)
	}
	return nil
}

// UnkeptTimes returns when each title was last unkept, keyed like the
// keepers. A title never unkept has no entry.
func (s *Store) UnkeptTimes() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT ref, unkept_at FROM unkept`)
	if err != nil {
		return nil, fmt.Errorf("reading unkeep times: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var ref, at string
		if err := rows.Scan(&ref, &at); err != nil {
			return nil, fmt.Errorf("reading unkeep times: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("reading unkeep time of %s: %w", ref, err)
		}
		out[ref] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading unkeep times: %w", err)
	}
	return out, nil
}

// saveLocked writes the whole state to the database in one transaction.
// Caller must hold s.mu.
func (s *Store) saveLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	defer tx.Rollback() // a no-op once committed

	st := s.state.Settings
	if _, err := tx.Exec(`INSERT OR REPLACE INTO settings (id, log_level, poll_schedule, movies_grace_period,
		tv_grace_period, daemon_enabled, remove_ended_series, keep_tag, jellyfin_users)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.LogLevel, st.PollSchedule, st.MoviesGracePeriod, st.TVGracePeriod,
		st.DaemonEnabled, st.RemoveEndedSeries, st.KeepTag, st.JellyfinUsers); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	for service, c := range s.state.Connections.byService() {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO connections (service, url, api_key) VALUES (?, ?, ?)`,
			service, c.URL, c.APIKey); err != nil {
			return fmt.Errorf("saving connections: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM keepers`); err != nil {
		return fmt.Errorf("saving keepers: %w", err)
	}
	for ref, k := range s.state.Keepers {
		if _, err := tx.Exec(`INSERT INTO keepers (ref, sub, name) VALUES (?, ?, ?)`, ref, k.Sub, k.Name); err != nil {
			return fmt.Errorf("saving keepers: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}
