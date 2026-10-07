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

// Pre-dashboard env var names are no longer recognised — only
// REAPARR_SETTING_<KEY> pins a field.
func TestResolve_LegacyEnvVarNamesIgnored(t *testing.T) {
	t.Setenv("DELETE_MOVIES_AFTER", "1d")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("JELLYFIN_API_KEY", "legacy-key")

	got := Resolve(store.Settings{MoviesGracePeriod: "7d", LogLevel: "info"})
	if got.Settings.MoviesGracePeriod != "7d" || got.IsManaged("movies_grace_period") {
		t.Error("DELETE_MOVIES_AFTER still pins movies_grace_period")
	}
	if got.Settings.LogLevel != "info" || got.IsManaged("log_level") {
		t.Error("LOG_LEVEL still pins log_level")
	}

	conns := ResolveConnections(store.Connections{})
	if conns.Connections.Jellyfin.APIKey != "" || conns.IsManaged("jellyfin.api_key") {
		t.Error("JELLYFIN_API_KEY still pins jellyfin.api_key")
	}
}

func TestEnvVar(t *testing.T) {
	for key, want := range map[string]string{
		"log_level":           "REAPARR_SETTING_LOG_LEVEL",
		"poll_schedule":       "REAPARR_SETTING_POLL_SCHEDULE",
		"movies_grace_period": "REAPARR_SETTING_MOVIES_GRACE_PERIOD",
		"tv_grace_period":     "REAPARR_SETTING_TV_GRACE_PERIOD",
		"jellyfin.url":        "REAPARR_SETTING_JELLYFIN_URL",
		"seerr.api_key":       "REAPARR_SETTING_SEERR_API_KEY",
	} {
		if got := EnvVar(key); got != want {
			t.Errorf("EnvVar(%q) = %q, want %q", key, got, want)
		}
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
	t.Setenv("REAPARR_SETTING_JELLYFIN_URL", "http://jellyfin.internal:8096")
	t.Setenv("REAPARR_SETTING_JELLYFIN_API_KEY", "env-key")

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

// A URL pinned by env must not lock the API key: it stays editable and its
// persisted value (entered in the dashboard) is used.
func TestResolveConnections_EnvURLOnly_APIKeyFromStore(t *testing.T) {
	t.Setenv("REAPARR_SETTING_SEERR_URL", "http://seerr.internal:5055")

	got := ResolveConnections(store.Connections{
		Seerr: store.Connection{URL: "http://seerr:5055", APIKey: "ui-key"},
	})

	if got.Connections.Seerr.URL != "http://seerr.internal:5055" {
		t.Errorf("Seerr.URL = %q, want env-pinned value", got.Connections.Seerr.URL)
	}
	if got.Connections.Seerr.APIKey != "ui-key" {
		t.Errorf("Seerr.APIKey = %q, want persisted value", got.Connections.Seerr.APIKey)
	}
	if !got.IsManaged("seerr.url") {
		t.Error("seerr.url not reported as env-managed")
	}
	if got.IsManaged("seerr.api_key") {
		t.Error("seerr.api_key reported as env-managed with only REAPARR_SETTING_SEERR_URL set")
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
