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
		min      auth.Role
		wantCode int
		wantRan  bool
	}{
		{"nobody signed in is 401", fakeAuth{ok: false}, auth.User, http.StatusUnauthorized, false},
		{"no access is 403 even for the lowest guard", fakeAuth{p: auth.Principal{Role: auth.None}, ok: true}, auth.User, http.StatusForbidden, false},
		{"user passes a user guard", fakeAuth{p: auth.Principal{Role: auth.User}, ok: true}, auth.User, http.StatusNoContent, true},
		{"user fails a curator guard", fakeAuth{p: auth.Principal{Role: auth.User}, ok: true}, auth.Curator, http.StatusForbidden, false},
		{"curator passes a curator guard", fakeAuth{p: auth.Principal{Role: auth.Curator}, ok: true}, auth.Curator, http.StatusNoContent, true},
		{"curator fails an admin guard", fakeAuth{p: auth.Principal{Role: auth.Curator}, ok: true}, auth.Admin, http.StatusForbidden, false},
		{"admin passes every guard", fakeAuth{p: auth.Principal{Role: auth.Admin}, ok: true}, auth.Admin, http.StatusNoContent, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, called := ran()
			rec := httptest.NewRecorder()
			serverWith(tc.auth).guard(tc.min, h)(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if *called != tc.wantRan {
				t.Fatalf("handler ran = %v, want %v", *called, tc.wantRan)
			}
		})
	}
}

// TestRouteGuards pins which role each route needs, so a route can't be
// added or loosened without this table changing with it.
func TestRouteGuards(t *testing.T) {
	routes := map[string]auth.Role{
		"/api/me":                  auth.None, // anyone signed in, so the page can say "no access"
		"/api/status":              auth.User,
		"/api/due":                 auth.User,
		"/api/library":             auth.User,
		"/api/library/keep":        auth.User,
		"/api/poster/radarr/1":     auth.User,
		"/api/due/delete":          auth.Curator,
		"/api/due/delete-selected": auth.Curator,
		"/api/settings":            auth.Admin,
		"/api/connections":         auth.Admin,
		"/api/connections/test":    auth.Admin,
		"/api/jellyfin/users":      auth.Admin,
	}
	roles := []auth.Role{auth.None, auth.User, auth.Curator, auth.Admin}
	for path, min := range routes {
		for _, role := range roles {
			mux := http.NewServeMux()
			// Handlers behind a passed guard would touch the nil sweeper/store;
			// a POST with an empty body (or GET on a POST-only route) fails
			// before that, with anything but 401/403. That is all this needs.
			serverWith(fakeAuth{p: auth.Principal{Role: role}, ok: true}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			blocked := rec.Code == http.StatusForbidden
			if role.AtLeast(min) && blocked {
				t.Errorf("%s: %s should pass a %s guard, got 403", path, role, min)
			}
			if !role.AtLeast(min) && !blocked {
				t.Errorf("%s: %s should be blocked by a %s guard, got %d", path, role, min, rec.Code)
			}
		}
	}

	mux := http.NewServeMux()
	serverWith(fakeAuth{ok: false}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz must stay open, got %d", rec.Code)
	}
}

func TestMe(t *testing.T) {
	mux := http.NewServeMux()
	serverWith(fakeAuth{p: auth.Principal{Sub: "s", Name: "Søren", Role: auth.Curator}, ok: true}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var me map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me["role"] != "curator" || me["name"] != "Søren" {
		t.Fatalf("unexpected me: %v", me)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("/api/me must not be cached")
	}

	mux = http.NewServeMux()
	serverWith(fakeAuth{ok: false}).Routes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nobody signed in should be 401, got %d", rec.Code)
	}
}
