package main

import (
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
	}, srv
}

// Deletion scope is the whole point of this tool — see
// watch-cleanup-tool-plan.md: Radarr/Sonarr only, deleteFiles=true so the
// Radarr/Sonarr-side hardlink is dropped, and it must never touch
// qBittorrent. These tests assert the exact request shape sent.
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
