// Package settings implements reaparr's env-var override convention,
// mirroring automouse's internal/settings package in this same stack
// (Resolve/IsManaged/MaskSecret), split into two resolvers: tunables (log
// level, poll schedule, grace periods) and connections (Jellyfin/Radarr/
// Sonarr/Seerr URL + API key).
//
// Every field is pinned by exactly one env var, REAPARR_SETTING_<KEY>,
// derived from the field's key (see EnvVar): e.g. "tv_grace_period" ->
// REAPARR_SETTING_TV_GRACE_PERIOD, "jellyfin.api_key" ->
// REAPARR_SETTING_JELLYFIN_API_KEY. If that env var is set, it wins over
// the persisted store and the field is reported as env-managed so the UI
// can render it read-only with the env var's name. If not set, the
// persisted value applies and is editable from the UI.
package settings

import (
	"os"
	"strconv"
	"strings"

	"github.com/Miista/reaparr/internal/store"
)

const envPrefix = "REAPARR_SETTING_"

// EnvVar returns the env var that pins the field with the given key.
func EnvVar(key string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// envManaged reports whether key is currently overridden by its env var,
// returning the raw value and the env var's name for UI display.
func envManaged(key string) (value string, envVar string, managed bool) {
	envVar = EnvVar(key)
	if v, ok := os.LookupEnv(envVar); ok && v != "" {
		return v, envVar, true
	}
	return "", envVar, false
}

// Resolved is the effective settings view: persisted values with any env
// overrides applied, plus per-field metadata for the UI.
type Resolved struct {
	Settings store.Settings
	Managed  map[string]string // key -> env var name, only present when env-managed
}

// Resolve merges persisted settings with any active REAPARR_SETTING_* env
// overrides.
func Resolve(persisted store.Settings) Resolved {
	r := Resolved{Settings: persisted, Managed: map[string]string{}}

	apply := func(key string, assign func(string)) {
		if v, envVar, ok := envManaged(key); ok {
			assign(v)
			r.Managed[key] = envVar
		}
	}

	apply("log_level", func(v string) { r.Settings.LogLevel = v })
	apply("poll_schedule", func(v string) { r.Settings.PollSchedule = v })
	apply("movies_grace_period", func(v string) { r.Settings.MoviesGracePeriod = v })
	apply("tv_grace_period", func(v string) { r.Settings.TVGracePeriod = v })
	apply("keep_tag", func(v string) { r.Settings.KeepTag = v })
	apply("jellyfin_users", func(v string) { r.Settings.JellyfinUsers = v })
	// An unparseable value disables the daemon: failing safe means not
	// deleting anything automatically.
	apply("daemon_enabled", func(v string) {
		enabled, err := strconv.ParseBool(v)
		r.Settings.DaemonEnabled = err == nil && enabled
	})

	return r
}

// IsManaged reports whether the given key is currently env-managed in this
// resolved settings view.
func (r Resolved) IsManaged(key string) bool {
	_, ok := r.Managed[key]
	return ok
}

// ResolvedConnections is the effective connections view, mirroring Resolved
// above but for store.Connections.
type ResolvedConnections struct {
	Connections store.Connections
	Managed     map[string]string // key -> env var name, only present when env-managed
}

// ResolveConnections merges persisted connections with any active
// REAPARR_SETTING_* env overrides (e.g. REAPARR_SETTING_JELLYFIN_URL).
func ResolveConnections(persisted store.Connections) ResolvedConnections {
	r := ResolvedConnections{Connections: persisted, Managed: map[string]string{}}

	apply := func(key string, assign func(string)) {
		if v, envVar, ok := envManaged(key); ok {
			assign(v)
			r.Managed[key] = envVar
		}
	}

	apply("jellyfin.url", func(v string) { r.Connections.Jellyfin.URL = v })
	apply("jellyfin.api_key", func(v string) { r.Connections.Jellyfin.APIKey = v })
	apply("radarr.url", func(v string) { r.Connections.Radarr.URL = v })
	apply("radarr.api_key", func(v string) { r.Connections.Radarr.APIKey = v })
	apply("sonarr.url", func(v string) { r.Connections.Sonarr.URL = v })
	apply("sonarr.api_key", func(v string) { r.Connections.Sonarr.APIKey = v })
	apply("seerr.url", func(v string) { r.Connections.Seerr.URL = v })
	apply("seerr.api_key", func(v string) { r.Connections.Seerr.APIKey = v })

	return r
}

// IsManaged reports whether the given key is currently env-managed in this
// resolved connections view.
func (r ResolvedConnections) IsManaged(key string) bool {
	_, ok := r.Managed[key]
	return ok
}

// MaskSecret returns a masked representation of a secret value, showing
// only the last 4 characters, e.g. "••••••1234". Empty values are returned
// empty. Identical to automouse's MaskSecret.
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	const visible = 4
	if len(value) <= visible {
		return strings.Repeat("•", len(value))
	}
	return strings.Repeat("•", len(value)-visible) + value[len(value)-visible:]
}
