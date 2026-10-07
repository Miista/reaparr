// Package settings implements reaparr's env-var override convention,
// mirroring automouse's internal/settings package in this same stack
// (Field/Resolve/IsManaged/MaskSecret), split into two resolvers:
//
//   - Tunables (log level, poll schedule, grace periods) are pinned via
//     REAPARR_SETTING_<KEY>, a reaparr-specific prefix for values that have
//     no pre-existing env var of their own.
//   - Connections (Jellyfin/Radarr/Sonarr/Seerr URL + API key) are pinned
//     via the EXACT existing env var names already used by config.go
//     (JELLYFIN_URL, JELLYFIN_API_KEY, ...) — not renamed, so a running
//     deployment's existing environment continues to lock the same fields
//     it always has, with no migration needed.
//
// In both cases: if an env var is set, it wins over the persisted store and
// the field is reported as env-managed so the UI can render it read-only
// with the controlling env var's name. If not set via env, the persisted
// value applies and is editable from the UI.
package settings

import (
	"os"
	"strings"

	"github.com/Miista/reaparr/internal/store"
)

const envPrefix = "REAPARR_SETTING_"

// Field describes one overridable setting or connection field. EnvVars is
// checked in order — first one set wins — since reaparr's four tunables
// each already had a long-standing env var name (LOG_LEVEL, POLL_SCHEDULE,
// DELETE_MOVIES_AFTER, DELETE_TV_SHOWS_AFTER) before this dashboard
// existed, and those must keep locking the field exactly as before, not
// merely seed its initial persisted value once. REAPARR_SETTING_<KEY> is
// offered as an additional, more consistent alias alongside them.
type Field struct {
	Key     string
	EnvVars []string
}

// settingsDefinitions is the full list of tunables that participate in the
// env-override mechanism — each checked against its pre-existing env var
// name first, then its REAPARR_SETTING_ alias.
var settingsDefinitions = []Field{
	{Key: "log_level", EnvVars: []string{"LOG_LEVEL", envPrefix + "LOG_LEVEL"}},
	{Key: "poll_schedule", EnvVars: []string{"POLL_SCHEDULE", envPrefix + "POLL_SCHEDULE"}},
	{Key: "movies_grace_period", EnvVars: []string{"DELETE_MOVIES_AFTER", envPrefix + "MOVIES_GRACE_PERIOD"}},
	{Key: "tv_grace_period", EnvVars: []string{"DELETE_TV_SHOWS_AFTER", envPrefix + "TV_GRACE_PERIOD"}},
}

// connectionDefinitions maps each connection field to the EXACT existing
// env var name from config.go's loadConfig — see this package's doc
// comment for why these are not renamed.
var connectionDefinitions = []Field{
	{Key: "jellyfin.url", EnvVars: []string{"JELLYFIN_URL"}},
	{Key: "jellyfin.api_key", EnvVars: []string{"JELLYFIN_API_KEY"}},
	{Key: "radarr.url", EnvVars: []string{"RADARR_URL"}},
	{Key: "radarr.api_key", EnvVars: []string{"RADARR_API_KEY"}},
	{Key: "sonarr.url", EnvVars: []string{"SONARR_URL"}},
	{Key: "sonarr.api_key", EnvVars: []string{"SONARR_API_KEY"}},
	{Key: "seerr.url", EnvVars: []string{"SEERR_URL"}},
	{Key: "seerr.api_key", EnvVars: []string{"SEERR_API_KEY"}},
}

// SettingsDefinitions returns the full list of tunable-setting field
// definitions.
func SettingsDefinitions() []Field { return settingsDefinitions }

// ConnectionDefinitions returns the full list of connection field
// definitions.
func ConnectionDefinitions() []Field { return connectionDefinitions }

// envManaged reports whether key is currently overridden by an environment
// variable in the given definition set — checking each of that field's
// EnvVars in order, first one set wins — and returns the raw string value
// and which env var name is actually in effect, for UI display.
func envManaged(defs []Field, key string) (value string, envVar string, managed bool) {
	for _, f := range defs {
		if f.Key != key {
			continue
		}
		for _, ev := range f.EnvVars {
			if v, ok := os.LookupEnv(ev); ok && v != "" {
				return v, ev, true
			}
		}
		if len(f.EnvVars) > 0 {
			return "", f.EnvVars[0], false
		}
		return "", "", false
	}
	return "", "", false
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
		if v, envVar, ok := envManaged(settingsDefinitions, key); ok {
			assign(v)
			r.Managed[key] = envVar
		}
	}

	apply("log_level", func(v string) { r.Settings.LogLevel = v })
	apply("poll_schedule", func(v string) { r.Settings.PollSchedule = v })
	apply("movies_grace_period", func(v string) { r.Settings.MoviesGracePeriod = v })
	apply("tv_grace_period", func(v string) { r.Settings.TVGracePeriod = v })

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
// JELLYFIN_URL/JELLYFIN_API_KEY/etc env overrides — the exact existing
// config.go env vars.
func ResolveConnections(persisted store.Connections) ResolvedConnections {
	r := ResolvedConnections{Connections: persisted, Managed: map[string]string{}}

	apply := func(key string, assign func(string)) {
		if v, envVar, ok := envManaged(connectionDefinitions, key); ok {
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
