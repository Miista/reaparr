package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.json")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	return s
}

func TestDuePending_ExcludesFutureWatched(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Not due yet", User: "a", WatchedAt: now,
	}))

	due, err := s.duePending(now.Add(-time.Hour)) // cutoff before watched_at
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected 0 due items, got %d", len(due))
	}
}

func TestDuePending_IncludesPastGracePeriod(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	watchedAt := now.Add(-48 * time.Hour)

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Due", User: "a", WatchedAt: watchedAt,
	}))

	due, err := s.duePending(now.Add(-24 * time.Hour)) // cutoff after watched_at
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due item, got %d", len(due))
	}
	if due[0].ItemID != "1" || due[0].Title != "Due" {
		t.Fatalf("unexpected item: %+v", due[0])
	}
}

func TestDuePending_ExcludesAlreadyActioned(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	watchedAt := now.Add(-48 * time.Hour)

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Already done", User: "a", WatchedAt: watchedAt,
	}))
	must(t, s.markActioned(kindMovie, "1", now))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected 0 due items (already actioned), got %d", len(due))
	}
}

// A show polled via multiple episodes upserts the same (kind, item_id)
// repeatedly (once per episode marked played) — watched_at must collapse to
// the earliest one seen, not the latest, so the grace period starts from
// whichever episode was watched first.
func TestUpsertWatched_KeepsEarliestWatchedAt(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindSeries, ItemID: "42", Title: "Show", User: "a", WatchedAt: now.Add(-24 * time.Hour),
	}))
	must(t, s.upsertWatched(watchedItem{
		Kind: kindSeries, ItemID: "42", Title: "Show", User: "a", WatchedAt: now.Add(-72 * time.Hour),
	}))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 item, got %d: %+v", len(due), due)
	}
	if !due[0].WatchedAt.Equal(now.Add(-72 * time.Hour)) {
		t.Fatalf("expected earliest watched_at to be kept, got %v", due[0].WatchedAt)
	}
}

func TestUpsertWatched_LaterPollDoesNotAdvanceWatchedAt(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Movie", User: "a", WatchedAt: now.Add(-72 * time.Hour),
	}))
	// A later poll observes the same item again (still played) with a more
	// recent WatchedAt e.g. due to a rewatch — must not push the grace
	// clock forward.
	must(t, s.upsertWatched(watchedItem{
		Kind: kindMovie, ItemID: "1", Title: "Movie", User: "a", WatchedAt: now,
	}))

	due, err := s.duePending(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected item to still be due based on earliest watched_at, got %d", len(due))
	}
}

func TestMarkActioned_OnlyAffectsMatchingKindAndItem(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	must(t, s.upsertWatched(watchedItem{Kind: kindMovie, ItemID: "1", Title: "Movie", User: "a", WatchedAt: now}))
	must(t, s.upsertWatched(watchedItem{Kind: kindSeries, ItemID: "1", Title: "Series", User: "a", WatchedAt: now}))

	must(t, s.markActioned(kindMovie, "1", now))

	due, err := s.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 || due[0].Kind != kindSeries {
		t.Fatalf("expected only the series item still pending, got %+v", due)
	}
}

// PersistsAcrossReload is the key correctness property of the flat-file
// store — the JSON file must survive a process restart (a fresh openStore
// against the same path) with all fields, including actioned_at, intact.
func TestStore_PersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.json")
	now := time.Now().UTC().Truncate(time.Second) // JSON round-trips to second precision via RFC3339

	s1, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	must(t, s1.upsertWatched(watchedItem{Kind: kindMovie, ItemID: "1", Title: "Movie", User: "a", WatchedAt: now.Add(-48 * time.Hour)}))
	must(t, s1.upsertWatched(watchedItem{Kind: kindSeries, ItemID: "2", Title: "Show", User: "b", WatchedAt: now.Add(-72 * time.Hour)}))
	must(t, s1.markActioned(kindSeries, "2", now))

	s2, err := openStore(path)
	if err != nil {
		t.Fatalf("re-openStore: %v", err)
	}

	due, err := s2.duePending(now)
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 || due[0].ItemID != "1" {
		t.Fatalf("expected only the movie still pending after reload, got %+v", due)
	}
}

func TestStore_OpenMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore on missing file should succeed, got: %v", err)
	}
	due, err := s.duePending(time.Now().UTC())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected empty store, got %d items", len(due))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
