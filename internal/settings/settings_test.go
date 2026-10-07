package settings

import (
	"testing"

	"github.com/Miista/reaparr/internal/store"
)

func TestResolve_NoEnvVars_ReturnsPersistedUnchanged(t *testing.T) {
	persisted := store.Settings{
		LogLevel:          "debug",
		PollSchedule:      "@daily",
		MoviesGracePeriod: "3d",
		TVGracePeriod:     "10d",
	}
	got := Resolve(persisted)
	if got.Settings != persisted {
		t.Errorf("Settings = %+v, want unchanged %+v", got.Settings, persisted)
	}
	for _, key := range []string{"log_level", "poll_schedule", "movies_grace_period", "tv_grace_period"} {
		if got.IsManaged(key) {
			t.Errorf("%s reported as managed with no env var set", key)
		}
	}
}

func TestResolve_EnvVarWins(t *testing.T) {
	t.Setenv("REAPARR_SETTING_MOVIES_GRACE_PERIOD", "1d")
	t.Setenv("REAPARR_SETTING_LOG_LEVEL", "warn")

	got := Resolve(store.Settings{LogLevel: "info", MoviesGracePeriod: "7d", TVGracePeriod: "7d"})

	if got.Settings.MoviesGracePeriod != "1d" {
		t.Errorf("MoviesGracePeriod = %q, want env-pinned 1d", got.Settings.MoviesGracePeriod)
	}
	if got.Settings.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want env-pinned warn", got.Settings.LogLevel)
	}
	if got.Settings.TVGracePeriod != "7d" {
		t.Errorf("TVGracePeriod = %q, want untouched persisted 7d", got.Settings.TVGracePeriod)
	}
	if !got.IsManaged("movies_grace_period") {
		t.Error("movies_grace_period not reported as env-managed")
	}
	if !got.IsManaged("log_level") {
		t.Error("log_level not reported as env-managed")
	}
	if got.IsManaged("tv_grace_period") {
		t.Error("tv_grace_period reported as managed with no env var set")
	}
}

// TestResolve_LegacyEnvVarNamesStillLock pins the exact regression found
// manually against a running container: DELETE_MOVIES_AFTER (reaparr's
// original, pre-dashboard env var) must keep locking the field — not just
// seed its persisted value once — otherwise a POST to /api/settings could
// silently overwrite an operator's env-pinned grace period.
func TestResolve_LegacyEnvVarNamesStillLock(t *testing.T) {
	t.Setenv("DELETE_MOVIES_AFTER", "1d")
	t.Setenv("DELETE_TV_SHOWS_AFTER", "14d")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("POLL_SCHEDULE", "@daily")

	got := Resolve(store.Settings{MoviesGracePeriod: "7d", TVGracePeriod: "7d", LogLevel: "info", PollSchedule: "@hourly"})

	if got.Settings.MoviesGracePeriod != "1d" {
		t.Errorf("MoviesGracePeriod = %q, want env-pinned 1d", got.Settings.MoviesGracePeriod)
	}
	if !got.IsManaged("movies_grace_period") {
		t.Error("movies_grace_period not reported as env-managed via legacy DELETE_MOVIES_AFTER")
	}
	if got.Managed["movies_grace_period"] != "DELETE_MOVIES_AFTER" {
		t.Errorf("Managed[movies_grace_period] = %q, want DELETE_MOVIES_AFTER", got.Managed["movies_grace_period"])
	}
	if !got.IsManaged("tv_grace_period") || !got.IsManaged("log_level") || !got.IsManaged("poll_schedule") {
		t.Error("expected all four legacy-named env vars to lock their fields")
	}
}

func TestResolve_EmptyEnvVarDoesNotCountAsSet(t *testing.T) {
	t.Setenv("REAPARR_SETTING_LOG_LEVEL", "")
	got := Resolve(store.Settings{LogLevel: "info"})
	if got.Settings.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want persisted info (empty env var should not override)", got.Settings.LogLevel)
	}
	if got.IsManaged("log_level") {
		t.Error("log_level reported as managed from an empty env var")
	}
}

func TestResolveConnections_NoEnvVars_ReturnsPersistedUnchanged(t *testing.T) {
	persisted := store.Connections{
		Jellyfin: store.Connection{URL: "http://jellyfin:8096", APIKey: "jf-key"},
		Radarr:   store.Connection{URL: "http://radarr:7878", APIKey: "r-key"},
	}
	got := ResolveConnections(persisted)
	if got.Connections != persisted {
		t.Errorf("Connections = %+v, want unchanged %+v", got.Connections, persisted)
	}
	if got.IsManaged("jellyfin.url") || got.IsManaged("radarr.api_key") {
		t.Error("fields reported as managed with no env vars set")
	}
}

func TestResolveConnections_EnvVarWins(t *testing.T) {
	t.Setenv("JELLYFIN_URL", "http://jellyfin.internal:8096")
	t.Setenv("JELLYFIN_API_KEY", "env-key")

	got := ResolveConnections(store.Connections{
		Jellyfin: store.Connection{URL: "http://jellyfin:8096", APIKey: "persisted-key"},
		Radarr:   store.Connection{URL: "http://radarr:7878", APIKey: "persisted-radarr-key"},
	})

	if got.Connections.Jellyfin.URL != "http://jellyfin.internal:8096" {
		t.Errorf("Jellyfin.URL = %q, want env-pinned value", got.Connections.Jellyfin.URL)
	}
	if got.Connections.Jellyfin.APIKey != "env-key" {
		t.Errorf("Jellyfin.APIKey = %q, want env-pinned value", got.Connections.Jellyfin.APIKey)
	}
	if got.Connections.Radarr.APIKey != "persisted-radarr-key" {
		t.Errorf("Radarr.APIKey = %q, want untouched persisted value", got.Connections.Radarr.APIKey)
	}
	if !got.IsManaged("jellyfin.url") || !got.IsManaged("jellyfin.api_key") {
		t.Error("jellyfin fields not reported as env-managed")
	}
	if got.IsManaged("radarr.url") || got.IsManaged("radarr.api_key") {
		t.Error("radarr fields reported as managed with no env var set")
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "•"},
		{"abcd", "••••"},
		{"abcde", "•bcde"},
		{"0123456789", "••••••6789"},
	}
	for _, tc := range tests {
		if got := MaskSecret(tc.in); got != tc.want {
			t.Errorf("MaskSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
