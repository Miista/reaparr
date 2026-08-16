package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// mediaKind distinguishes which *arr API owns the item.
type mediaKind string

const (
	kindMovie  mediaKind = "movie"
	kindSeries mediaKind = "series"
)

// watchedItem is the current known watched-state for one item, keyed by
// (kind, item_id). WatchedAt comes from Jellyfin's own
// UserData.LastPlayedDate rather than "when we first polled it" — more
// accurate, and Jellyfin already tracks it so there's no reason to duplicate
// that clock ourselves.
type watchedItem struct {
	Kind       mediaKind  `json:"kind"`
	ItemID     string     `json:"item_id"`
	Title      string     `json:"title"`
	User       string     `json:"user"`
	WatchedAt  time.Time  `json:"watched_at"`
	ActionedAt *time.Time `json:"actioned_at,omitempty"`
}

func itemKey(kind mediaKind, itemID string) string {
	return string(kind) + ":" + itemID
}

// store is a flat key-value store persisted as a single JSON file. Given
// the actual access pattern here — get/set by key, iterate-and-filter by
// time — a relational database is unwarranted; this is a household's
// watched list, not high-throughput. Polling replaced the earlier
// webhook-listener design, so there is also no concurrent writer to guard
// against beyond the single sweep loop itself; the mutex exists purely for
// safety, not because concurrent access is expected.
type store struct {
	path  string
	mu    sync.Mutex
	items map[string]watchedItem
}

func openStore(path string) (*store, error) {
	s := &store{path: path, items: make(map[string]watchedItem)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.items); err != nil {
		return nil, fmt.Errorf("parse store: %w", err)
	}
	return s, nil
}

// save writes the current state atomically (temp file + rename) so a
// process crash mid-write can never leave a corrupt/partial store file.
func (s *store) save() error {
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".watch-cleanup-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

// upsertWatched records the earliest known watched_at for (kind, item_id).
// Called once per polled item per sweep. If the item is already actioned,
// its actioned_at is left untouched — a rewatch after cleanup doesn't
// resurrect it (the file is already gone; Radarr/Sonarr would need to
// re-grab it, which is a decision for that system, not this one).
func (s *store) upsertWatched(w watchedItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := itemKey(w.Kind, w.ItemID)
	if existing, ok := s.items[key]; ok {
		w.ActionedAt = existing.ActionedAt
		if existing.WatchedAt.Before(w.WatchedAt) {
			w.WatchedAt = existing.WatchedAt
		}
	}
	s.items[key] = w
	return s.save()
}

// duePending returns every not-yet-actioned watched item whose watched_at
// is at or before the cutoff.
func (s *store) duePending(cutoff time.Time) ([]watchedItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var due []watchedItem
	for _, w := range s.items {
		if w.ActionedAt != nil {
			continue
		}
		if w.WatchedAt.After(cutoff) {
			continue
		}
		due = append(due, w)
	}
	return due, nil
}

// markActioned marks (kind, item_id) as done, so it doesn't get reprocessed.
func (s *store) markActioned(kind mediaKind, itemID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := itemKey(kind, itemID)
	w, ok := s.items[key]
	if !ok || w.ActionedAt != nil {
		return nil
	}
	w.ActionedAt = &at
	s.items[key] = w
	return s.save()
}
