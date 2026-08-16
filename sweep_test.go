package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeJellyfin serves a fixed set of users and, for each, whatever
// played items are provided keyed by user ID. A nil/missing entry serves an
// empty list, which is enough for tests that only care about the sweep's
// deletion side and don't need polling to surface anything new.
func newFakeJellyfin(t *testing.T, itemsByUser map[string][]jellyfinItem) *jellyfinClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			var users []jellyfinUser
			for id := range itemsByUser {
				users = append(users, jellyfinUser{ID: id, Name: id})
			}
			if len(itemsByUser) == 0 {
				users = []jellyfinUser{{ID: "u1", Name: "u1"}}
			}
			json.NewEncoder(w).Encode(users)
		default:
			// /Users/{id}/Items
			userID := r.URL.Path[len("/Users/") : len(r.URL.Path)-len("/Items")]
			json.NewEncoder(w).Encode(jellyfinItemsResponse{Items: itemsByUser[userID]})
		}
	}))
	t.Cleanup(srv.Close)
	return &jellyfinClient{baseURL: srv.URL, apiKey: "k", httpClient: srv.Client(), log: testLogger(t)}
}

func TestSweepOnce_DeletesDueMovieAndMarksActioned(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Old Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	var deleteCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: newFakeJellyfin(t, nil), arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected 1 delete call, got %d", got)
	}

	due, err := s.duePending(now.Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected item marked actioned after sweep, still pending: %+v", due)
	}
}

func TestSweepOnce_LeavesNotYetDueItemsAlone(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Recent Movie", User: "a", WatchedAt: now,
	}))

	var deleteCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: newFakeJellyfin(t, nil), arr: arr, gracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 delete calls before grace period elapses, got %d", got)
	}
}

// If Radarr/Sonarr returns an error, the item must stay pending so a later
// sweep retries it — a failed delete must never be silently marked done.
func TestSweepOnce_FailedDeleteLeavesItemPending(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Old Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: newFakeJellyfin(t, nil), arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	due, err := s.duePending(now.Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected item to remain pending after failed delete, got %d", len(due))
	}
}

func TestSweepOnce_RoutesSeriesToSonarr(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindSeries, ItemID: "7", Title: "Old Show", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	var gotPath string
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("radarr should not be called for a series item")
	}))
	defer radarrSrv.Close()
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: radarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: sonarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: newFakeJellyfin(t, nil), arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if gotPath != "/api/v3/series/7" {
		t.Fatalf("sonarr path = %q, want /api/v3/series/7", gotPath)
	}
}

func TestSweepOnce_PollsJellyfinAndRecordsNewPlayedMovie(t *testing.T) {
	s := newTestStore(t)

	items := map[string][]jellyfinItem{
		"u1": {
			{ID: "abc", Name: "Freshly Watched", Type: "Movie", UserData: jellyfinUserData{Played: true, LastPlayedDate: time.Now().UTC().Format(time.RFC3339)}},
		},
	}

	arrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("arr should not be called — grace period has not elapsed")
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "k", sonarrURL: arrSrv.URL, sonarrAPIKey: "k", httpClient: arrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: newFakeJellyfin(t, items), arr: arr, gracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	// A cutoff before the grace period has elapsed: the freshly-polled item
	// must not show up yet.
	due, err := s.duePending(time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected freshly-polled item to not be due yet, got %d", len(due))
	}

	// A cutoff past the grace period: it should exist and eventually become
	// due once the grace period elapses.
	dueFuture, err := s.duePending(time.Now().UTC().Add(30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(dueFuture) != 1 || dueFuture[0].Title != "Freshly Watched" {
		t.Fatalf("expected polled movie to be recorded, got %+v", dueFuture)
	}
}

func TestSweepOnce_PollFailureDoesNotBlockDeletionOfKnownItems(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Old Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	brokenJellyfin := &jellyfinClient{baseURL: "http://127.0.0.1:0", apiKey: "k", httpClient: http.DefaultClient, log: testLogger(t)}

	var deleteCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, jellyfin: brokenJellyfin, arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected deletion of already-known item to proceed despite poll failure, got %d delete calls", got)
	}
}
