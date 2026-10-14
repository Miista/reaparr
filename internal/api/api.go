// Package api implements reaparr's HTTP handlers: settings and
// connections (with env-override reporting, mirroring automouse's
// settings API), a dashboard preview of items due for deletion, and manual
// per-item delete/skip actions. The login itself is authy's, wrapped around
// the whole server in main.go (see the Login section of this repo's README).
// What a signed-in person may do is decided here, per route, from their role
// (see internal/auth): a route is guarded in its handler's path, not in the
// page, since a hidden button is not access control. With the login turned
// off (REAPARR_AUTH_DISABLED=true) everyone is an admin and this dashboard is
// meant for a LAN or a proxy that authenticates for it.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/auth"
	"github.com/Miista/reaparr/internal/settings"
	"github.com/Miista/reaparr/internal/store"
)

// Authorizer says who a request is from. Satisfied by *auth.Auth.
type Authorizer interface {
	Principal(*http.Request) (auth.Principal, bool)
}

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
	// Library lists every movie and series in Radarr/Sonarr.
	Library() ([]LibraryItem, error)
	// JellyfinUsers lists the accounts on the Jellyfin server, for the
	// dashboard's user picker.
	JellyfinUsers() ([]JellyfinUser, error)
	// SetKept keeps (keep=true) or unkeeps Radarr movies / Sonarr series.
	SetKept(refs []LibraryRef, keep bool) error
	// Kept reports whether each given title carries the keep tag right now,
	// reading only the services the titles belong to.
	Kept(refs []LibraryRef) (map[LibraryRef]bool, error)
	// Poster returns a movie's / series' small poster image.
	Poster(service string, id int) (io.ReadCloser, string, error)
}

// LibraryItem is one movie (Radarr) or series (Sonarr) in the library.
type LibraryItem struct {
	Service string `json:"service"` // "radarr" or "sonarr"
	ID      int    `json:"id"`      // Radarr movie / Sonarr series ID
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Kind    string `json:"kind"` // "movie" or "series"
	Kept    bool   `json:"kept"` // carries the keep tag: never deleted
	// Watch is what unkeeping would do: "due" (would be deleted on the next
	// scheduled run), "waiting", "unwatched", or "" if unknown.
	Watch string `json:"watch"`
	// CanUnkeep and KeptBy are filled in per request (see handleLibrary), not
	// by the sweeper: whether the person asking may unkeep this kept title,
	// and, for admins only, who kept it.
	CanUnkeep bool   `json:"can_unkeep"`
	KeptBy    string `json:"kept_by,omitempty"`
}

// JellyfinUser is one Jellyfin account, as offered by the user picker.
type JellyfinUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LibraryRef identifies a Radarr movie / Sonarr series.
type LibraryRef struct {
	Service string `json:"service"`
	ID      int    `json:"id"`
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
	ArrService  string `json:"arr_service,omitempty"` // "radarr"/"sonarr" when resolved — for the poster
	ArrID       int    `json:"arr_id,omitempty"`
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
	auth    Authorizer
	log     zerolog.Logger

	// keepMu serialises keep/unkeep: each request reads which titles are kept,
	// decides who may change what, then changes the tags and the keeper
	// records, and two of those interleaving could disagree about who owns a
	// title.
	keepMu sync.Mutex
}

// New builds a Server.
func New(st *store.Store, sw Sweeper, tester ConnectionTester, reload ReloadFunc, authn Authorizer, log zerolog.Logger) *Server {
	return &Server{
		store:   st,
		sweeper: sw,
		tester:  tester,
		reload:  reload,
		auth:    authn,
		log:     log.With().Str("component", "api").Logger(),
	}
}

// Routes registers all handlers on mux. Each route lists the roles it
// accepts; a person needs any one of them. internal/api/guard_test.go pins
// this table, and the page mirrors it (MAY in web/static/app.js).
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/me", s.handleMe)
	// Seeing what is due and keeping things.
	view := []auth.Role{auth.User, auth.Admin}
	mux.HandleFunc("/api/status", s.guard(view, s.handleStatus))
	mux.HandleFunc("/api/due", s.guard(view, s.handleDue))
	mux.HandleFunc("/api/library", s.guard(view, s.handleLibrary))
	mux.HandleFunc("/api/library/keep", s.guard(view, s.handleLibraryKeep))
	mux.HandleFunc("/api/poster/", s.guard(view, s.handlePoster))
	// Deleting by hand. The scheduled sweep is not a request and is
	// unaffected by roles.
	del := []auth.Role{auth.Admin}
	mux.HandleFunc("/api/due/delete", s.guard(del, s.handleDeleteDue))
	mux.HandleFunc("/api/due/delete-selected", s.guard(del, s.handleDeleteSelected))
	// Settings and connections, including reading them (URLs and masked
	// keys are still configuration).
	configure := []auth.Role{auth.Admin}
	mux.HandleFunc("/api/settings", s.guard(configure, s.handleSettings))
	mux.HandleFunc("/api/connections", s.guard(configure, s.handleConnections))
	mux.HandleFunc("/api/connections/test", s.guard(configure, s.handleTestConnection))
	mux.HandleFunc("/api/jellyfin/users", s.guard(configure, s.handleJellyfinUsers))
}

type principalKey struct{}

// guard runs h only for someone signed in who holds one of roles. Nobody
// signed in is 401, so the page knows to sign in; signed in but not allowed
// is 403.
func (s *Server) guard(roles []auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.auth.Principal(r)
		switch {
		case !ok:
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		case !p.HasAny(roles...):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "You do not have access to this"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

// who is the signed-in person behind a guarded request, for the log.
func who(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return p
}

// handleMe tells the page who is signed in and what to show. It is open to
// anyone signed in, including someone with no access, so the page can say so
// instead of failing every call.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p, ok := s.auth.Principal(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	roles := p.Roles
	if roles == nil {
		roles = []auth.Role{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"sub":   p.Sub,
		"name":  p.Name,
		"email": p.Email,
		"roles": roles,
		"local": p.Local,
	})
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

// handlePoster proxies a poster from Radarr/Sonarr: GET
// /api/poster/{radarr|sonarr}/{id}. Proxying keeps the API keys server-side
// and the browser off the internet; posters rarely change, so they're
// cached for a day.
func (s *Server) handlePoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/poster/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, contentType, err := s.sweeper.Poster(parts[0], id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer body.Close()
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.Copy(w, body)
}

// handleLibrary lists every movie and series in Radarr/Sonarr.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	items, err := s.sweeper.Library()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.annotateKeepers(who(r), items)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// refKey is how a title is named in the keeper records.
func refKey(service string, id int) string { return service + ":" + strconv.Itoa(id) }

// keepers returns a copy of the keeper records.
func (s *Server) keepers() map[string]store.Keeper {
	out := map[string]store.Keeper{}
	s.store.View(func(st store.State) {
		for k, v := range st.Keepers {
			out[k] = v
		}
	})
	return out
}

// mayUnkeep: an admin may unkeep any title; anyone else only one they kept
// themselves. A kept title with no recorded owner (kept before owners were
// recorded, or by hand in Radarr/Sonarr) is therefore an admin's to unkeep.
func mayUnkeep(p auth.Principal, owner store.Keeper, owned bool) bool {
	return p.Has(auth.Admin) || (owned && owner.Sub != "" && owner.Sub == p.Sub)
}

// annotateKeepers fills in, for the person asking, which kept titles they may
// unkeep and, for admins only, who kept each. The library is otherwise the
// same for everyone.
func (s *Server) annotateKeepers(p auth.Principal, items []LibraryItem) {
	owners := s.keepers()
	admin := p.Has(auth.Admin)
	for i := range items {
		if !items[i].Kept {
			continue
		}
		owner, owned := owners[refKey(items[i].Service, items[i].ID)]
		items[i].CanUnkeep = mayUnkeep(p, owner, owned)
		if admin && owned {
			items[i].KeptBy = owner.Name
			if items[i].KeptBy == "" {
				items[i].KeptBy = owner.Sub
			}
		}
	}
}

// handleJellyfinUsers lists the Jellyfin accounts for the user picker.
func (s *Server) handleJellyfinUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	users, err := s.sweeper.JellyfinUsers()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// setKeepers records k as the keeper of every title in keys.
func (s *Server) setKeepers(keys []string, k store.Keeper) error {
	return s.store.Update(func(st *store.State) {
		if st.Keepers == nil {
			st.Keepers = map[string]store.Keeper{}
		}
		for _, key := range keys {
			st.Keepers[key] = k
		}
	})
}

// dropKeepers forgets who kept each title in keys.
func (s *Server) dropKeepers(keys []string) error {
	return s.store.Update(func(st *store.State) {
		for _, key := range keys {
			delete(st.Keepers, key)
		}
	})
}

// handleLibraryKeep keeps or unkeeps library items.
func (s *Server) handleLibraryKeep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Items []LibraryRef `json:"items"`
		Keep  bool         `json:"keep"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "items is required"})
		return
	}
	for _, ref := range req.Items {
		if ref.Service != "radarr" && ref.Service != "sonarr" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown service %q", ref.Service)})
			return
		}
	}
	p := who(r)

	s.keepMu.Lock()
	defer s.keepMu.Unlock()

	// What is kept right now decides what each item means: keeping a kept
	// title changes nothing (least of all its owner), and unkeeping one that is
	// not kept changes nothing. Only the titles asked about are read.
	state, err := s.sweeper.Kept(req.Items)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	kept := make(map[string]bool, len(state))
	for ref, k := range state {
		if k {
			kept[refKey(ref.Service, ref.ID)] = true
		}
	}
	owners := s.keepers()

	var change, refused []LibraryRef
	var changed, stale []string // keeper-record keys
	seen := map[string]bool{}
	unknown := 0
	for _, ref := range req.Items {
		k := refKey(ref.Service, ref.ID)
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, found := state[ref]; !found {
			unknown++ // not in the library: nothing to keep, and no record to trust or drop
			continue
		}
		owner, owned := owners[k]
		switch {
		case req.Keep && kept[k]:
			// already kept
		case !req.Keep && !kept[k]:
			if owned {
				stale = append(stale, k) // the tag is gone; so is the reason to remember who kept it
			}
		case req.Keep, mayUnkeep(p, owner, owned):
			change = append(change, ref)
			changed = append(changed, k)
		default:
			refused = append(refused, ref)
		}
	}

	if unknown == len(seen) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such title in the library"})
		return
	}

	// Keeping records the owner first and then sets the tag, undoing the record
	// if the tag fails: a tag with no owner would lock the keeper out, a record
	// with no tag is harmless. Unkeeping goes the other way, tag first; a record
	// left behind by a failed cleanup is dropped by the next unkeep or keep.
	if req.Keep && len(changed) > 0 {
		if err := s.setKeepers(changed, store.Keeper{Sub: p.Sub, Name: p.Name}); err != nil {
			s.log.Error().Err(err).Msg("recording who kept a title")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "recording who kept it failed; nothing was kept"})
			return
		}
	}
	if len(change) > 0 {
		if err := s.sweeper.SetKept(change, req.Keep); err != nil {
			if req.Keep {
				if derr := s.dropKeepers(changed); derr != nil {
					s.log.Error().Err(derr).Msg("forgetting who kept a title that was not kept")
				}
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.log.Info().Str("by", p.Sub).Bool("keep", req.Keep).Int("titles", len(change)).Msg("kept titles changed")
	}
	if !req.Keep && len(changed)+len(stale) > 0 {
		if err := s.dropKeepers(append(changed, stale...)); err != nil {
			s.log.Error().Err(err).Msg("forgetting who kept a title")
		}
	}

	switch {
	case len(refused) > 0 && len(change) == 0:
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "Only whoever kept this, or an admin, can unkeep it",
			"refused": refused,
		})
	case len(refused) > 0:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "refused": refused})
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
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
	s.log.Info().Str("by", who(r).Sub).Int("deleted", result.Deleted).Int("skipped", result.Skipped).Int("failed", result.Failed).Msg("manual delete of selected items")
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
	if err := s.validateSettingsPatch(incoming); err != nil {
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

// validateSettingsPatch rejects grace periods that don't parse or are below
// settings.MinGracePeriod, before anything is saved. Env-managed fields are
// skipped — applySettingsPatch ignores them anyway.
func (s *Server) validateSettingsPatch(incoming map[string]any) error {
	var st store.State
	s.store.View(func(state store.State) { st = state })
	resolved := settings.Resolve(st.Settings)

	for _, key := range []string{"movies_grace_period", "tv_grace_period"} {
		v, ok := incoming[key].(string)
		if !ok || resolved.IsManaged(key) {
			continue
		}
		if _, err := settings.ValidateGracePeriod(strings.TrimSpace(v)); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
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
	s.log.Info().Str("by", who(r).Sub).Str("id", req.ID).Msg("manual delete")
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
	setString("jellyfin_users", &st.JellyfinUsers)

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
			"jellyfin_users":      st.JellyfinUsers,
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
