package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSweepOnce_DeletesDueMovieAndMarksActioned(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Old Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	var deleteCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected 1 delete call, got %d", got)
	}

	due, err := s.duePending(now.Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected event marked actioned after sweep, still pending: %+v", due)
	}
}

func TestSweepOnce_LeavesNotYetDueEventsAlone(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Recent Movie", User: "a", WatchedAt: now,
	}))

	var deleteCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deleteCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, arr: arr, gracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 delete calls before grace period elapses, got %d", got)
	}
}

// If Radarr/Sonarr returns an error, the event must stay pending so a later
// sweep retries it — a failed delete must never be silently marked done.
func TestSweepOnce_FailedDeleteLeavesEventPending(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Old Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	due, err := s.duePending(now.Add(24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected event to remain pending after failed delete, got %d", len(due))
	}
}

func TestSweepOnce_RoutesSeriesToSonarr(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindSeries, ItemID: "7", Title: "Old Show", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	var gotPath string
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("radarr should not be called for a series event")
	}))
	defer radarrSrv.Close()
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: radarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: sonarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{store: s, arr: arr, gracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if gotPath != "/api/v3/series/7" {
		t.Fatalf("sonarr path = %q, want /api/v3/series/7", gotPath)
	}
}
