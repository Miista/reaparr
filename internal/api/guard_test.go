package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/auth"
)

// fakeAuth answers every request with one fixed principal.
type fakeAuth struct {
	p  auth.Principal
	ok bool
}

func (f fakeAuth) Principal(*http.Request) (auth.Principal, bool) { return f.p, f.ok }

func serverWith(a Authorizer) *Server {
	return &Server{auth: a, log: zerolog.Nop()}
}

func holding(roles ...auth.Role) fakeAuth {
	return fakeAuth{p: auth.Principal{Roles: roles}, ok: true}
}

func TestGuard(t *testing.T) {
	ran := func() (http.HandlerFunc, *bool) {
		called := false
		return func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}, &called
	}

	tests := []struct {
		name     string
		auth     Authorizer
		accepts  []auth.Role
		wantCode int
		wantRan  bool
	}{
		{"nobody signed in is 401", fakeAuth{ok: false}, []auth.Role{auth.User}, http.StatusUnauthorized, false},
		{"no roles is 403", holding(), []auth.Role{auth.User, auth.Admin}, http.StatusForbidden, false},
		{"a held role passes", holding(auth.User), []auth.Role{auth.User}, http.StatusNoContent, true},
		{"any listed role is enough", holding(auth.Admin), []auth.Role{auth.User, auth.Admin}, http.StatusNoContent, true},
		{"a role not listed fails", holding(auth.User), []auth.Role{auth.Admin}, http.StatusForbidden, false},
		{"admin is not implied", holding(auth.Admin), []auth.Role{auth.User}, http.StatusForbidden, false},
		{"nothing accepted blocks everyone", holding(auth.Admin), nil, http.StatusForbidden, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, called := ran()
			rec := httptest.NewRecorder()
			serverWith(tc.auth).guard(tc.accepts, h)(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if *called != tc.wantRan {
				t.Fatalf("handler ran = %v, want %v", *called, tc.wantRan)
			}
		})
	}
}

// TestRouteGuards pins which roles each route accepts, so a route can't be
// added or loosened without this table changing with it.
func TestRouteGuards(t *testing.T) {
	view := []auth.Role{auth.User, auth.Admin}
	del := []auth.Role{auth.Admin}
	configure := []auth.Role{auth.Admin}
	routes := map[string][]auth.Role{
		"/api/status":              view,
		"/api/due":                 view,
		"/api/library":             view,
		"/api/library/keep":        view,
		"/api/poster/radarr/1":     view,
		"/api/due/delete":          del,
		"/api/due/delete-selected": del,
		"/api/settings":            configure,
		"/api/connections":         configure,
		"/api/connections/test":    configure,
		"/api/jellyfin/users":      configure,
	}
	// Each person holds exactly one role, plus one holding none.
	people := map[string]auth.Principal{
		"nobody": {},
		"user":   {Roles: []auth.Role{auth.User}},
		"admin":  {Roles: []auth.Role{auth.Admin}},
	}
	for path, accepts := range routes {
		for who, p := range people {
			mux := http.NewServeMux()
			// Handlers behind a passed guard would touch the nil sweeper/store;
			// a POST with an empty body (or GET on a POST-only route) fails
			// before that, with anything but 401/403. That is all this needs.
			serverWith(fakeAuth{p: p, ok: true}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			blocked := rec.Code == http.StatusForbidden
			if p.HasAny(accepts...) && blocked {
				t.Errorf("%s: %s should pass, got 403", path, who)
			}
			if !p.HasAny(accepts...) && !blocked {
				t.Errorf("%s: %s should be blocked, got %d", path, who, rec.Code)
			}
		}
	}

	// /api/me is for anyone signed in, even with no roles, so the page can
	// say "no access".
	mux := http.NewServeMux()
	serverWith(holding()).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/api/me with no roles should be 200, got %d", rec.Code)
	}

	mux = http.NewServeMux()
	serverWith(fakeAuth{ok: false}).Routes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz must stay open, got %d", rec.Code)
	}
}

func TestMe(t *testing.T) {
	mux := http.NewServeMux()
	serverWith(fakeAuth{p: auth.Principal{Sub: "s", Name: "Alex", Roles: []auth.Role{auth.User, auth.Admin}}, ok: true}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var me struct {
		Name  string   `json:"name"`
		Roles []string `json:"roles"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Name != "Alex" || len(me.Roles) != 2 || me.Roles[0] != "user" || me.Roles[1] != "admin" {
		t.Fatalf("unexpected me: %+v", me)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("/api/me must not be cached")
	}

	// No roles is an empty list, never null, so the page can treat it as one.
	mux = http.NewServeMux()
	serverWith(holding()).Routes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(rec.Body).Decode(&raw)
	if string(raw["roles"]) != "[]" {
		t.Fatalf("roles = %s, want []", raw["roles"])
	}

	mux = http.NewServeMux()
	serverWith(fakeAuth{ok: false}).Routes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nobody signed in should be 401, got %d", rec.Code)
	}
}
