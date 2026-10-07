// Package store persists reaparr's user-configurable settings and
// connections to a single JSON file under a data directory, mirroring
// automouse's internal/store package in this same stack. All writes go
// through Update, which rewrites the whole file atomically
// (temp-file-then-rename) with 0600 permissions since it holds API keys.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	// KeepTag is the Radarr/Sonarr tag that marks a movie or series as a
	// keeper: reaparr never deletes anything carrying it.
	KeepTag string `json:"keep_tag"`
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

// State is the full persisted document.
type State struct {
	Settings    Settings    `json:"settings"`
	Connections Connections `json:"connections"`
}

// Store guards State with a mutex and persists it to dataDir/config.json.
type Store struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	state   State
}

// Open loads state from dataDir/config.json, creating the directory and a
// fresh default document if none exists yet.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	s := &Store{
		path:    filepath.Join(dataDir, "config.json"),
		dataDir: dataDir,
		state: State{
			Settings:    DefaultSettings(),
			Connections: DefaultConnections(),
		},
	}
	if _, err := os.Stat(s.path); err == nil {
		raw, err := os.ReadFile(s.path)
		if err != nil {
			return nil, fmt.Errorf("reading state file: %w", err)
		}
		if err := json.Unmarshal(raw, &s.state); err != nil {
			return nil, fmt.Errorf("parsing state file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat state file: %w", err)
	} else {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// DataDir returns the directory state is persisted under.
func (s *Store) DataDir() string {
	return s.dataDir
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

// saveLocked writes state to disk. Caller must hold s.mu.
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}
	return nil
}
