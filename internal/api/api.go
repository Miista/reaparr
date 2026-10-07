// Package api implements reaparr's HTTP handlers: settings and
// connections (with env-override reporting, mirroring automouse's
// settings API), a dashboard preview of items due for deletion, and manual
// per-item delete/skip actions. No authentication — this dashboard is
// LAN-only, matching this stack's trust model for Maintainerr/qBittorrent
// WebUI (see this repo's README for the explicit tradeoff and the planned
// follow-up).
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/settings"
	"github.com/Miista/reaparr/internal/store"
)

// Sweeper is the subset of the main package's *sweeper this package needs —
// defined here (rather than importing "main", which Go disallows) so the
// dashboard's preview/manual-delete endpoints can share the exact same
// find/delete logic as the cron daemon. See main.go for the adapter that
// satisfies this interface.
type Sweeper interface {
	// FindDue returns every item currently due for deletion, rendered as
	// plain data (no reference back to internal sweeper types) ready for
	// JSON encoding.
	FindDue() ([]DueItem, error)
	// Delete deletes one specific due movie or season, identified by the ID
	// previously returned in a DueItem.
	Delete(id string) error
	// MissingServices lists the required services that aren't configured;
	// while non-empty, the sweep refuses to run.
	MissingServices() []string
	// DeleteSelected re-checks every candidate live and deletes the given
	// IDs among them, regardless of grace period or whether the daemon is
	// enabled.
	DeleteSelected(ids []string) (DeleteResult, error)
	// DaemonEnabled reports whether the scheduled sweep is enabled.
	DaemonEnabled() bool
	// NextRun is when the next scheduled sweep fires (if the daemon is
	// enabled).
	NextRun() time.Time
	// RefreshPreview rebuilds the cached due list from live data now.
	RefreshPreview()
	// Keep marks a listed item as a keeper (a season keeps its series).
	Keep(id string) error
	// KeepSelected is Keep for several items; returns how many
	// movies/series were tagged.
	KeepSelected(ids []string) (int, error)
	// Kept lists every keeper.
	Kept() ([]KeptItem, error)
	// Unkeep removes keeper status from a Radarr movie / Sonarr series.
	Unkeep(service string, id int) error
}

// KeptItem is a movie or series marked as a keeper (never deleted).
type KeptItem struct {
	Service string `json:"service"` // "radarr" or "sonarr"
	ID      int    `json:"id"`      // Radarr movie / Sonarr series ID
	Title   string `json:"title"`
	Kind    string `json:"kind"` // "movie" or "series"
}

// DeleteResult summarises a manual multi-item delete.
type DeleteResult struct {
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"` // due, but no radarr/sonarr match
	Failed  int `json:"failed"`
}

// DueItem is a dashboard-facing rendering of one movie or season due for
// deletion.
type DueItem struct {
	ID          string `json:"id"` // a movie's Jellyfin item ID, or "season:<seriesId>:<n>"
	Title       string `json:"title"`
	Kind        string `json:"kind"` // "movie" or "season"
	GracePeriod string `json:"grace_period"`
	StoppedAt   string `json:"stopped_at"` // RFC3339; for a season, its latest episode stop
	DueAt       string `json:"due_at"`     // RFC3339; stopped_at + grace period
	Due         bool   `json:"due"`        // past its grace period; false => still waiting
	Resolved    bool   `json:"resolved"`   // false => can't be deleted; Reason says why
	Reason      string `json:"reason,omitempty"`
}

// ConnectionTester tests a single configured connection and reports whether
// it currently works — built fresh from whatever values are passed (not
// necessarily yet saved), so the dashboard's "Test connection" button can
// validate a field before the user saves it.
type ConnectionTester interface {
	TestJellyfin(url, apiKey string) error
	TestRadarr(url, apiKey string) error
	TestSonarr(url, apiKey string) error
	TestSeerr(url, apiKey string) error
}

// ReloadFunc rebuilds the live sweeper's clients from the store's current
// resolved state — called after any settings/connections save so a change
// takes effect on the next sweep without a container restart.
type ReloadFunc func()

// Server wires the store, sweeper, and connection tester into HTTP
// handlers.
type Server struct {
	store   *store.Store
	sweeper Sweeper
	tester  ConnectionTester
	reload  ReloadFunc
	log     zerolog.Logger
}

// New builds a Server.
func New(st *store.Store, sw Sweeper, tester ConnectionTester, reload ReloadFunc, log zerolog.Logger) *Server {
	return &Server{
		store:   st,
		sweeper: sw,
		tester:  tester,
		reload:  reload,
		log:     log.With().Str("component", "api").Logger(),
	}
}

// Routes registers all handlers on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/connections", s.handleConnections)
	mux.HandleFunc("/api/connections/test", s.handleTestConnection)
	mux.HandleFunc("/api/due", s.handleDue)
	mux.HandleFunc("/api/due/delete", s.handleDeleteDue)
	mux.HandleFunc("/api/due/delete-selected", s.handleDeleteSelected)
	mux.HandleFunc("/api/keep", s.handleKeep)
	mux.HandleFunc("/api/keep-selected", s.handleKeepSelected)
	mux.HandleFunc("/api/kept", s.handleKept)
	mux.HandleFunc("/api/unkeep", s.handleUnkeep)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleStatus reports whether the sweep can run, i.e. which required
// services (if any) are still unconfigured — drives the dashboard banner.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	missing := s.sweeper.MissingServices()
	if missing == nil {
		missing = []string{}
	}
	status := map[string]any{
		"missing_services": missing,
		"daemon_enabled":   s.sweeper.DaemonEnabled(),
	}
	if s.sweeper.DaemonEnabled() {
		status["next_run"] = s.sweeper.NextRun().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, status)
}

// handleKeep marks one listed item as a keeper.
func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if err := s.sweeper.Keep(req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleKeepSelected marks several listed items as keepers.
func (s *Server) handleKeepSelected(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil || len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids is required"})
		return
	}
	tagged, err := s.sweeper.KeepSelected(req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"kept": tagged})
}

// handleKept lists every keeper.
func (s *Server) handleKept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	kept, err := s.sweeper.Kept()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kept": kept})
}

// handleUnkeep removes keeper status from one movie/series.
func (s *Server) handleUnkeep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Service string `json:"service"`
		ID      int    `json:"id"`
	}
	if err := readJSON(r, &req); err != nil || req.Service == "" || req.ID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "service and id are required"})
		return
	}
	if err := s.sweeper.Unkeep(req.Service, req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteSelected deletes the given IDs (each re-checked live).
func (s *Server) handleDeleteSelected(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids is required"})
		return
	}
	result, err := s.sweeper.DeleteSelected(req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSettings handles GET (current resolved settings) and POST (patch
// editable fields; env-managed fields are silently ignored so a client
// can't override an operator-pinned value via the API).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var st store.State
		s.store.View(func(state store.State) { st = state })
		writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var incoming map[string]any
	if err := readJSON(r, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := s.store.Update(func(st *store.State) {
		applySettingsPatch(&st.Settings, incoming)
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.reload()

	var st store.State
	s.store.View(func(state store.State) { st = state })
	writeJSON(w, http.StatusOK, publicSettings(settings.Resolve(st.Settings)))
}

// handleConnections handles GET (current resolved connections, secrets
// masked) and POST (patch editable fields; env-managed fields are silently
// ignored).
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var st store.State
		s.store.View(func(state store.State) { st = state })
		writeJSON(w, http.StatusOK, publicConnections(settings.ResolveConnections(st.Connections)))
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var incoming map[string]any
	if err := readJSON(r, &incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if err := s.store.Update(func(st *store.State) {
		applyConnectionsPatch(&st.Connections, incoming)
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.reload()

	var st store.State
	s.store.View(func(state store.State) { st = state })
	writeJSON(w, http.StatusOK, publicConnections(settings.ResolveConnections(st.Connections)))
}

// handleTestConnection tests a single service's connection using whatever
// url/api_key is posted — not necessarily what's currently saved, so a
// user can validate before saving. An env-managed field's current
// (resolved) value is used if the request omits it, since the UI renders
// that field read-only and has no value of its own to send.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Service string `json:"service"` // "jellyfin" | "radarr" | "sonarr" | "seerr"
		URL     string `json:"url"`
		APIKey  string `json:"api_key"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var st store.State
	s.store.View(func(state store.State) { st = state })
	resolved := settings.ResolveConnections(st.Connections)

	url, apiKey := strings.TrimSpace(req.URL), strings.TrimSpace(req.APIKey)
	switch req.Service {
	case "jellyfin":
		if url == "" {
			url = resolved.Connections.Jellyfin.URL
		}
		if apiKey == "" {
			apiKey = resolved.Connections.Jellyfin.APIKey
		}
	case "radarr":
		if url == "" {
			url = resolved.Connections.Radarr.URL
		}
		if apiKey == "" {
			apiKey = resolved.Connections.Radarr.APIKey
		}
	case "sonarr":
		if url == "" {
			url = resolved.Connections.Sonarr.URL
		}
		if apiKey == "" {
			apiKey = resolved.Connections.Sonarr.APIKey
		}
	case "seerr":
		if url == "" {
			url = resolved.Connections.Seerr.URL
		}
		if apiKey == "" {
			apiKey = resolved.Connections.Seerr.APIKey
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown service " + req.Service})
		return
	}

	var err error
	switch req.Service {
	case "jellyfin":
		err = s.tester.TestJellyfin(url, apiKey)
	case "radarr":
		err = s.tester.TestRadarr(url, apiKey)
	case "sonarr":
		err = s.tester.TestSonarr(url, apiKey)
	case "seerr":
		err = s.tester.TestSeerr(url, apiKey)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDue renders the current set of items due for deletion, using the
// exact same matching the cron sweep uses — see Sweeper.FindDue.
func (s *Server) handleDue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// ?fresh=1 (the dashboard's Refresh button) rebuilds the list from live
	// data first instead of serving the cached preview.
	if r.URL.Query().Get("fresh") == "1" {
		s.sweeper.RefreshPreview()
	}
	due, err := s.sweeper.FindDue()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if due == nil {
		due = []DueItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"due": due})
}

// handleDeleteDue deletes one specific due movie or season, by the ID from
// a DueItem, via the same delete call the cron sweep uses.
func (s *Server) handleDeleteDue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if err := s.sweeper.Delete(req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applySettingsPatch applies whitelisted fields from incoming onto st.
// Fields that are currently env-managed are silently ignored so a client
// can't override an operator-pinned value via the API.
func applySettingsPatch(st *store.Settings, incoming map[string]any) {
	resolved := settings.Resolve(*st)

	setString := func(key string, dst *string) {
		if resolved.IsManaged(key) {
			return
		}
		if v, ok := incoming[key].(string); ok {
			*dst = strings.TrimSpace(v)
		}
	}

	setString("log_level", &st.LogLevel)
	setString("poll_schedule", &st.PollSchedule)
	setString("movies_grace_period", &st.MoviesGracePeriod)
	setString("tv_grace_period", &st.TVGracePeriod)
	setString("keep_tag", &st.KeepTag)

	if !resolved.IsManaged("daemon_enabled") {
		if v, ok := incoming["daemon_enabled"].(bool); ok {
			st.DaemonEnabled = v
		}
	}
}

// applyConnectionsPatch applies whitelisted fields from incoming onto st.
// incoming is expected to be nested per service, e.g.
// {"jellyfin": {"url": "...", "api_key": "..."}, ...} — matching
// publicConnections' own shape.
func applyConnectionsPatch(st *store.Connections, incoming map[string]any) {
	resolved := settings.ResolveConnections(*st)

	apply := func(service string, conn *store.Connection, urlKey, apiKeyKey string) {
		raw, ok := incoming[service].(map[string]any)
		if !ok {
			return
		}
		if !resolved.IsManaged(urlKey) {
			if v, ok := raw["url"].(string); ok {
				conn.URL = strings.TrimSpace(v)
			}
		}
		if !resolved.IsManaged(apiKeyKey) {
			if v, ok := raw["api_key"].(string); ok {
				// An empty string from the client means "no change" — the
				// GET response never echoes a real secret back (see
				// publicConnections' masking), so there is nothing a
				// client could legitimately submit here to intentionally
				// clear a key versus simply not having touched the field.
				// Trimmed: a pasted key with a stray space/newline would
				// otherwise be rejected as unauthorized.
				if v = strings.TrimSpace(v); v != "" {
					conn.APIKey = v
				}
			}
		}
	}

	apply("jellyfin", &st.Jellyfin, "jellyfin.url", "jellyfin.api_key")
	apply("radarr", &st.Radarr, "radarr.url", "radarr.api_key")
	apply("sonarr", &st.Sonarr, "sonarr.url", "sonarr.api_key")
	apply("seerr", &st.Seerr, "seerr.url", "seerr.api_key")
}

// publicSettings renders resolved settings for API responses: env-managed
// fields are flagged with the controlling env var's name.
func publicSettings(resolved settings.Resolved) map[string]any {
	st := resolved.Settings
	return map[string]any{
		"values": map[string]any{
			"log_level":           st.LogLevel,
			"poll_schedule":       st.PollSchedule,
			"movies_grace_period": st.MoviesGracePeriod,
			"tv_grace_period":     st.TVGracePeriod,
			"daemon_enabled":      st.DaemonEnabled,
			"keep_tag":            st.KeepTag,
		},
		"env_managed": resolved.Managed,
	}
}

// publicConnections renders resolved connections for API responses:
// env-managed fields are flagged, and every API key is masked — the raw
// value is never echoed back over the API once saved, whether it came from
// the environment or the persisted store.
func publicConnections(resolved settings.ResolvedConnections) map[string]any {
	c := resolved.Connections
	render := func(conn store.Connection) map[string]any {
		return map[string]any{
			"url":         conn.URL,
			"api_key":     settings.MaskSecret(conn.APIKey),
			"api_key_set": conn.APIKey != "",
		}
	}
	return map[string]any{
		"values": map[string]any{
			"jellyfin": render(c.Jellyfin),
			"radarr":   render(c.Radarr),
			"sonarr":   render(c.Sonarr),
			"seerr":    render(c.Seerr),
		},
		"env_managed": resolved.Managed,
	}
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
