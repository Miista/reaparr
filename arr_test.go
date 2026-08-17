package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func newTestArrClient(t *testing.T, handler http.HandlerFunc) (*arrClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &arrClient{
		radarrURL:    srv.URL,
		radarrAPIKey: "radarr-key",
		sonarrURL:    srv.URL,
		sonarrAPIKey: "sonarr-key",
		httpClient:   srv.Client(),
		log:          testLogger(t),
	}, srv
}

// Deletion scope is the whole point of this tool: Radarr/Sonarr only,
// deleteFiles=true so the Radarr/Sonarr-side hardlink is dropped, and it
// must never touch qBittorrent. These tests assert the exact request shape
// sent.
func TestDeleteMovie_RequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey string
	var gotQuery url.Values

	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})

	if err := client.deleteMovie("42"); err != nil {
		t.Fatalf("deleteMovie: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v3/movie/42" {
		t.Errorf("path = %q, want /api/v3/movie/42", gotPath)
	}
	if gotQuery.Get("deleteFiles") != "true" {
		t.Errorf("deleteFiles = %q, want true", gotQuery.Get("deleteFiles"))
	}
	if gotAPIKey != "radarr-key" {
		t.Errorf("api key = %q, want radarr-key", gotAPIKey)
	}
}

func TestDeleteSeries_RequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey string
	var gotQuery url.Values

	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})

	if err := client.deleteSeries("7"); err != nil {
		t.Fatalf("deleteSeries: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v3/series/7" {
		t.Errorf("path = %q, want /api/v3/series/7", gotPath)
	}
	if gotQuery.Get("deleteFiles") != "true" {
		t.Errorf("deleteFiles = %q, want true", gotQuery.Get("deleteFiles"))
	}
	if gotAPIKey != "sonarr-key" {
		t.Errorf("api key = %q, want sonarr-key", gotAPIKey)
	}
}

func TestDeleteMovie_NonSuccessStatusIsError(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if err := client.deleteMovie("999"); err == nil {
		t.Fatal("expected error on 404 response, got nil")
	}
}

// This is the exact bug hit in the first live test against a real stack:
// Jellyfin's own item ID was being passed straight to Radarr's delete API,
// which correctly rejected it with 404 since Radarr has no idea what that
// ID means. findMovieByTmdbID is the fix — matching on the ID both systems
// actually share.
func TestFindMovieByTmdbID_MatchesOnTmdbID(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]radarrMovie{
			{ID: 1, Title: "Other Movie", TmdbID: 111},
			{ID: 15, Title: "Minions & Monsters", TmdbID: 1315772},
		})
	})

	movie, ok, err := client.findMovieByTmdbID("1315772")
	if err != nil {
		t.Fatalf("findMovieByTmdbID: %v", err)
	}
	if !ok {
		t.Fatal("expected a match, got none")
	}
	if movie.ID != 15 {
		t.Fatalf("expected radarr id 15, got %d", movie.ID)
	}
}

func TestFindMovieByTmdbID_NoMatchReturnsOkFalse(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]radarrMovie{{ID: 1, Title: "Other Movie", TmdbID: 111}})
	})

	_, ok, err := client.findMovieByTmdbID("999999")
	if err != nil {
		t.Fatalf("findMovieByTmdbID: %v", err)
	}
	if ok {
		t.Fatal("expected no match, got ok=true")
	}
}

// A movies-only household has no use for Sonarr, and vice versa — either
// service being unconfigured must be a clean no-op (no request sent, no
// error), not a crash or a doomed HTTP call to an empty URL.
func TestFindMovieByTmdbID_RadarrNotConfigured_ReturnsOkFalseNoRequest(t *testing.T) {
	requestSent := false
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestSent = true
		w.WriteHeader(http.StatusOK)
	})
	client.radarrAPIKey = ""

	_, ok, err := client.findMovieByTmdbID("1315772")
	if err != nil {
		t.Fatalf("expected no error when radarr isn't configured, got: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when radarr isn't configured")
	}
	if requestSent {
		t.Fatal("expected no HTTP request to be sent when radarr isn't configured")
	}
}

func TestFindSeriesByTvdbID_SonarrNotConfigured_ReturnsOkFalseNoRequest(t *testing.T) {
	requestSent := false
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestSent = true
		w.WriteHeader(http.StatusOK)
	})
	client.sonarrAPIKey = ""

	_, ok, err := client.findSeriesByTvdbID("410092")
	if err != nil {
		t.Fatalf("expected no error when sonarr isn't configured, got: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when sonarr isn't configured")
	}
	if requestSent {
		t.Fatal("expected no HTTP request to be sent when sonarr isn't configured")
	}
}

func TestFindSeriesByTvdbID_MatchesOnTvdbID(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/series" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]sonarrSeries{
			{ID: 1, Title: "Other Show", TvdbID: 111},
			{ID: 7, Title: "Kaleidoscope", TvdbID: 410092},
		})
	})

	series, ok, err := client.findSeriesByTvdbID("410092")
	if err != nil {
		t.Fatalf("findSeriesByTvdbID: %v", err)
	}
	if !ok {
		t.Fatal("expected a match, got none")
	}
	if series.ID != 7 {
		t.Fatalf("expected sonarr id 7, got %d", series.ID)
	}
}

// This is the precondition for Reaparr's entire "downloads copy stays
// untouched" guarantee: if Radarr/Sonarr aren't configured to use
// hardlinks, deleting via their API deletes the only copy of the file.
func TestRadarrUsesHardlinks_ReflectsCopyUsingHardlinksSetting(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/config/mediamanagement" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
	})

	ok, err := client.radarrUsesHardlinks()
	if err != nil {
		t.Fatalf("radarrUsesHardlinks: %v", err)
	}
	if !ok {
		t.Fatal("expected true when copyUsingHardlinks is true")
	}
}

func TestRadarrUsesHardlinks_FalseWhenDisabled(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: false})
	})

	ok, err := client.radarrUsesHardlinks()
	if err != nil {
		t.Fatalf("radarrUsesHardlinks: %v", err)
	}
	if ok {
		t.Fatal("expected false when copyUsingHardlinks is false")
	}
}

func TestSonarrUsesHardlinks_ReflectsCopyUsingHardlinksSetting(t *testing.T) {
	client, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/config/mediamanagement" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
	})

	ok, err := client.sonarrUsesHardlinks()
	if err != nil {
		t.Fatalf("sonarrUsesHardlinks: %v", err)
	}
	if !ok {
		t.Fatal("expected true when copyUsingHardlinks is true")
	}
}
