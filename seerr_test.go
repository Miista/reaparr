package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestSeerrClient(t *testing.T, handler http.HandlerFunc) (*seerrClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &seerrClient{
		baseURL:    srv.URL,
		apiKey:     "seerr-key",
		httpClient: srv.Client(),
		log:        testLogger(t),
	}, srv
}

func TestHasSeerr_ReflectsAPIKeyPresence(t *testing.T) {
	client := &seerrClient{}
	if client.hasSeerr() {
		t.Fatal("expected hasSeerr=false with no api key")
	}
	client.apiKey = "set"
	if !client.hasSeerr() {
		t.Fatal("expected hasSeerr=true once api key is set")
	}
}

// deletedRequests must fetch a title per stale item, since the
// /request?filter=deleted response's nested media object has no title
// field itself — confirmed live against a real Seerr instance (see
// SEERR_PLAN.md).
func TestDeletedRequests_FetchesTitlePerItem(t *testing.T) {
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/request":
			if r.URL.Query().Get("filter") != "deleted" {
				t.Fatalf("expected filter=deleted, got %q", r.URL.Query().Get("filter"))
			}
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: 1},
				Results: []seerrRequest{
					{ID: 1, Media: seerrMedia{ID: 21, TmdbID: 1315772, Status: 7, MediaType: "movie"}},
				},
			})
		case "/api/v1/movie/1315772":
			json.NewEncoder(w).Encode(seerrTitledMedia{Title: "Minions & Monsters"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stale, err := client.deletedRequests()
	if err != nil {
		t.Fatalf("deletedRequests: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale item, got %d", len(stale))
	}
	if stale[0].mediaID != 21 {
		t.Errorf("mediaID = %d, want 21", stale[0].mediaID)
	}
	if stale[0].title != "Minions & Monsters" {
		t.Errorf("title = %q, want 'Minions & Monsters'", stale[0].title)
	}
}

func TestDeletedRequests_TvUsesNameField(t *testing.T) {
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/request":
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: 1},
				Results: []seerrRequest{
					{ID: 2, Media: seerrMedia{ID: 11, TmdbID: 410092, Status: 7, MediaType: "tv"}},
				},
			})
		case "/api/v1/tv/410092":
			json.NewEncoder(w).Encode(seerrTitledMedia{Name: "Kaleidoscope"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stale, err := client.deletedRequests()
	if err != nil {
		t.Fatalf("deletedRequests: %v", err)
	}
	if len(stale) != 1 || stale[0].title != "Kaleidoscope" {
		t.Fatalf("expected title 'Kaleidoscope', got %+v", stale)
	}
}

// Multiple requests can point at the same underlying media (see
// SEERR_PLAN.md) — deletedRequests must deduplicate by media ID so the
// same media isn't deleted twice, and so titleFor is only called once per
// distinct media record.
func TestDeletedRequests_DeduplicatesByMediaID(t *testing.T) {
	titleLookups := 0
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/request":
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: 2},
				Results: []seerrRequest{
					{ID: 1, Media: seerrMedia{ID: 21, TmdbID: 1315772, Status: 7, MediaType: "movie"}},
					{ID: 2, Media: seerrMedia{ID: 21, TmdbID: 1315772, Status: 7, MediaType: "movie"}},
				},
			})
		case "/api/v1/movie/1315772":
			titleLookups++
			json.NewEncoder(w).Encode(seerrTitledMedia{Title: "Minions & Monsters"})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stale, err := client.deletedRequests()
	if err != nil {
		t.Fatalf("deletedRequests: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 deduplicated item, got %d", len(stale))
	}
	if titleLookups != 1 {
		t.Fatalf("expected exactly 1 title lookup, got %d", titleLookups)
	}
}

// If the title lookup itself fails, that must not fail the whole sweep —
// fall back to logging by media id instead.
func TestDeletedRequests_TitleLookupFailure_FallsBackToMediaID(t *testing.T) {
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/request":
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: 1},
				Results: []seerrRequest{
					{ID: 1, Media: seerrMedia{ID: 21, TmdbID: 1315772, Status: 7, MediaType: "movie"}},
				},
			})
		case "/api/v1/movie/1315772":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	})

	stale, err := client.deletedRequests()
	if err != nil {
		t.Fatalf("deletedRequests: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 item despite title lookup failure, got %d", len(stale))
	}
	if stale[0].title != "media id 21" {
		t.Errorf("expected fallback title 'media id 21', got %q", stale[0].title)
	}
}

func TestDeletedRequests_Pagination(t *testing.T) {
	const total = 3
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/movie/1" || r.URL.Path == "/api/v1/movie/2" || r.URL.Path == "/api/v1/movie/3" {
			json.NewEncoder(w).Encode(seerrTitledMedia{Title: "Title " + r.URL.Path})
			return
		}

		skip := 0
		fmt.Sscanf(r.URL.Query().Get("skip"), "%d", &skip)

		switch skip {
		case 0:
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: total},
				Results: []seerrRequest{
					{ID: 1, Media: seerrMedia{ID: 1, TmdbID: 1, MediaType: "movie"}},
					{ID: 2, Media: seerrMedia{ID: 2, TmdbID: 2, MediaType: "movie"}},
				},
			})
		case 2:
			json.NewEncoder(w).Encode(seerrRequestsResponse{
				PageInfo: seerrPageInfo{Results: total},
				Results: []seerrRequest{
					{ID: 3, Media: seerrMedia{ID: 3, TmdbID: 3, MediaType: "movie"}},
				},
			})
		default:
			t.Fatalf("unexpected skip: %d", skip)
		}
	})

	stale, err := client.deletedRequests()
	if err != nil {
		t.Fatalf("deletedRequests: %v", err)
	}
	if len(stale) != total {
		t.Fatalf("expected %d items across pages, got %d", total, len(stale))
	}
}

func TestDeleteMedia_RequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey string
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	})

	if err := client.deleteMedia(21); err != nil {
		t.Fatalf("deleteMedia: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/media/21" {
		t.Errorf("path = %q, want /api/v1/media/21", gotPath)
	}
	if gotAPIKey != "seerr-key" {
		t.Errorf("api key = %q, want seerr-key", gotAPIKey)
	}
}

func TestDeleteMedia_NonSuccessStatusIsError(t *testing.T) {
	client, _ := newTestSeerrClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if err := client.deleteMedia(999); err == nil {
		t.Fatal("expected error on 404 response, got nil")
	}
}
