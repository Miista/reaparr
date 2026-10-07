package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Jellyfin 12.x rejects the legacy X-Emby-Token header by default, so the
// key must go in the Authorization header's MediaBrowser scheme.
func TestJellyfinGet_UsesAuthorizationHeader(t *testing.T) {
	var gotAuth, gotLegacy string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotLegacy = r.Header.Get("X-Emby-Token")
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c := &jellyfinClient{baseURL: srv.URL, apiKey: "jf-key", httpClient: srv.Client(), log: testLogger(t)}
	if _, err := c.users(); err != nil {
		t.Fatalf("users: %v", err)
	}

	if want := `MediaBrowser Token="jf-key"`; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotLegacy != "" {
		t.Errorf("X-Emby-Token = %q, want unset", gotLegacy)
	}
}
