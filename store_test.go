package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDuePending_ExcludesFutureWatched(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Not due yet", User: "a", WatchedAt: now,
	}))

	due, err := s.duePending(now.Add(-time.Hour)) // cutoff before watched_at
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected 0 due events, got %d", len(due))
	}
}

func TestDuePending_IncludesPastGracePeriod(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	watchedAt := now.Add(-48 * time.Hour)

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Due", User: "a", WatchedAt: watchedAt,
	}))

	due, err := s.duePending(now.Add(-24 * time.Hour)) // cutoff after watched_at
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due event, got %d", len(due))
	}
	if due[0].ItemID != "1" || due[0].Title != "Due" {
		t.Fatalf("unexpected event: %+v", due[0])
	}
}

func TestDuePending_ExcludesAlreadyActioned(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	watchedAt := now.Add(-48 * time.Hour)

	must(t, s.recordWatched(watchedEvent{
		Kind: kindMovie, ItemID: "1", Title: "Already done", User: "a", WatchedAt: watchedAt,
	}))
	must(t, s.markActioned(kindMovie, "1", now))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected 0 due events (already actioned), got %d", len(due))
	}
}

// A show watched via multiple episodes produces multiple watched_events for
// the same (kind, item_id) — e.g. a season pack marked-played per episode.
// duePending must collapse these to one entry, keyed off the earliest
// watched_at, not fire per-episode duplicates for the sweeper.
func TestDuePending_CollapsesMultipleEventsPerItem(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{
		Kind: kindSeries, ItemID: "42", Title: "Show S1E1", User: "a", WatchedAt: now.Add(-72 * time.Hour),
	}))
	must(t, s.recordWatched(watchedEvent{
		Kind: kindSeries, ItemID: "42", Title: "Show S1E2", User: "a", WatchedAt: now.Add(-48 * time.Hour),
	}))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 collapsed event, got %d: %+v", len(due), due)
	}
}

func TestMarkActioned_OnlyAffectsMatchingKindAndItem(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.recordWatched(watchedEvent{Kind: kindMovie, ItemID: "1", Title: "Movie", User: "a", WatchedAt: now}))
	must(t, s.recordWatched(watchedEvent{Kind: kindSeries, ItemID: "1", Title: "Series", User: "a", WatchedAt: now}))

	must(t, s.markActioned(kindMovie, "1", now))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 || due[0].Kind != kindSeries {
		t.Fatalf("expected only the series event still pending, got %+v", due)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
