package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func state(s *Store) State {
	var st State
	s.View(func(v State) { st = v })
	return st
}

func writeLegacy(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, legacyFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return false
}

func TestFreshStoreHasDefaults(t *testing.T) {
	st := state(open(t, t.TempDir()))
	if st.Settings != DefaultSettings() {
		t.Errorf("settings = %+v, want defaults %+v", st.Settings, DefaultSettings())
	}
	if st.Connections != DefaultConnections() {
		t.Errorf("connections = %+v, want defaults %+v", st.Connections, DefaultConnections())
	}
	if len(st.Keepers) != 0 {
		t.Errorf("keepers = %v, want none", st.Keepers)
	}
}

func TestDatabaseIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)
	info, err := os.Stat(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode = %o, want 600 (it holds API keys)", mode)
	}
}

func TestUpdateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	err := s.Update(func(st *State) {
		st.Settings.MoviesGracePeriod = "14d"
		st.Settings.DaemonEnabled = false
		st.Settings.RemoveEndedSeries = true
		st.Connections.Radarr = Connection{URL: "http://radarr.example:7878", APIKey: "radarr-key"}
		st.Keepers = map[string]Keeper{
			"radarr:1": {Sub: "sub-alice", Name: "Alice"},
			"sonarr:2": {Sub: "sub-bob", Name: "Bob"},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := state(s)

	got := state(open(t, dir))
	if got.Settings != want.Settings {
		t.Errorf("settings = %+v, want %+v", got.Settings, want.Settings)
	}
	if got.Connections != want.Connections {
		t.Errorf("connections = %+v, want %+v", got.Connections, want.Connections)
	}
	if len(got.Keepers) != 2 || got.Keepers["radarr:1"] != want.Keepers["radarr:1"] || got.Keepers["sonarr:2"] != want.Keepers["sonarr:2"] {
		t.Errorf("keepers = %v, want %v", got.Keepers, want.Keepers)
	}
}

func TestRemovedKeeperStaysRemoved(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Update(func(st *State) {
		st.Keepers = map[string]Keeper{"radarr:1": {Sub: "sub-alice"}, "radarr:2": {Sub: "sub-bob"}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) { delete(st.Keepers, "radarr:1") }); err != nil {
		t.Fatal(err)
	}

	got := state(open(t, dir)).Keepers
	if _, ok := got["radarr:1"]; ok {
		t.Errorf("radarr:1 came back after being removed: %v", got)
	}
	if _, ok := got["radarr:2"]; !ok {
		t.Errorf("radarr:2 was lost: %v", got)
	}
}

func TestImportsLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	// No keep_tag or connections.seerr: an older file must keep those defaults.
	writeLegacy(t, dir, `{
		"settings": {"log_level": "debug", "poll_schedule": "@daily", "movies_grace_period": "3d",
			"tv_grace_period": "10d", "daemon_enabled": false, "remove_ended_series": true,
			"jellyfin_users": "alice,bob"},
		"connections": {"jellyfin": {"url": "http://jf.example:8096", "api_key": "jf-key"}},
		"keepers": {"radarr:7": {"sub": "sub-alice", "name": "Alice"}}
	}`)

	got := state(open(t, dir))
	want := Settings{
		LogLevel: "debug", PollSchedule: "@daily", MoviesGracePeriod: "3d", TVGracePeriod: "10d",
		DaemonEnabled: false, RemoveEndedSeries: true, KeepTag: "reaparr-keep", JellyfinUsers: "alice,bob",
	}
	if got.Settings != want {
		t.Errorf("settings = %+v, want %+v", got.Settings, want)
	}
	if got.Connections.Jellyfin != (Connection{URL: "http://jf.example:8096", APIKey: "jf-key"}) {
		t.Errorf("jellyfin connection = %+v", got.Connections.Jellyfin)
	}
	if got.Connections.Seerr != DefaultConnections().Seerr {
		t.Errorf("seerr connection = %+v, want the default", got.Connections.Seerr)
	}
	if got.Keepers["radarr:7"] != (Keeper{Sub: "sub-alice", Name: "Alice"}) {
		t.Errorf("keepers = %v", got.Keepers)
	}

	if exists(t, filepath.Join(dir, legacyFile)) {
		t.Error("config.json is still in place after the import")
	}
	if !exists(t, filepath.Join(dir, legacyFile+".migrated")) {
		t.Error("config.json.migrated is missing: the old file should be kept")
	}
}

func TestLegacyJSONIsImportedOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, dir, `{"settings": {"poll_schedule": "@daily"}}`)
	s := open(t, dir)
	if err := s.Update(func(st *State) { st.Settings.PollSchedule = "@weekly" }); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// A config.json that reappears (say, restored from an old backup) must
	// not overwrite what's in the database.
	writeLegacy(t, dir, `{"settings": {"poll_schedule": "@daily"}}`)
	if got := state(open(t, dir)).Settings.PollSchedule; got != "@weekly" {
		t.Errorf("poll schedule = %q, want the database's @weekly", got)
	}
	if !exists(t, filepath.Join(dir, legacyFile)) {
		t.Error("a config.json that wasn't imported was renamed anyway")
	}
}

func TestNoLegacyJSONLeavesNoMigratedFile(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)
	if exists(t, filepath.Join(dir, legacyFile+".migrated")) {
		t.Error("config.json.migrated was created without a config.json to import")
	}
}

func TestBrokenLegacyJSONFailsAndIsRetried(t *testing.T) {
	dir := t.TempDir()
	writeLegacy(t, dir, `{"settings": `)
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("Open succeeded with an unparseable config.json, want an error rather than silently using defaults")
	}
	if !exists(t, filepath.Join(dir, legacyFile)) {
		t.Fatal("an unparseable config.json was renamed: it would never be imported")
	}

	// Once fixed, the next start imports it: the failed attempt left no
	// state behind that would make it look done.
	writeLegacy(t, dir, `{"settings": {"poll_schedule": "@daily"}}`)
	if got := state(open(t, dir)).Settings.PollSchedule; got != "@daily" {
		t.Errorf("poll schedule = %q, want @daily from the fixed config.json", got)
	}
}

func TestUnkeptTimesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	first := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	if err := s.RecordUnkept([]string{"radarr:1", "sonarr:2"}, first); err != nil {
		t.Fatal(err)
	}
	later := first.Add(time.Hour)
	if err := s.RecordUnkept([]string{"radarr:1"}, later); err != nil {
		t.Fatal(err)
	}

	got, err := open(t, dir).UnkeptTimes()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"radarr:1": later, "sonarr:2": first}
	if len(got) != len(want) {
		t.Fatalf("unkeep times = %v, want %v", got, want)
	}
	for ref, w := range want {
		if !got[ref].Equal(w) {
			t.Errorf("%s unkept at %v, want %v", ref, got[ref], w)
		}
	}
}

func TestNoUnkeptTimesOnAFreshStore(t *testing.T) {
	got, err := open(t, t.TempDir()).UnkeptTimes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unkeep times = %v, want none", got)
	}
}

// A v1.4.0 database (schema version 1) is upgraded in place: what it held
// is still there, and unkeep times can be recorded.
func TestUpgradesAVersion1Database(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.Update(func(st *State) {
		st.Settings.PollSchedule = "@daily"
		st.Keepers = map[string]Keeper{"radarr:1": {Sub: "sub-alice", Name: "Alice"}}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE unkept; PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = open(t, dir)
	st := state(s)
	if st.Settings.PollSchedule != "@daily" || st.Keepers["radarr:1"].Sub != "sub-alice" {
		t.Fatalf("state after upgrade = %+v, want the version 1 data kept", st)
	}
	if err := s.RecordUnkept([]string{"radarr:1"}, time.Now()); err != nil {
		t.Fatalf("recording an unkeep after the upgrade: %v", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("schema version = %d, want %d", version, len(migrations))
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("Open accepted a schema newer than it knows")
	}
}
