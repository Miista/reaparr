package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// mediaKind distinguishes which *arr API owns the item.
type mediaKind string

const (
	kindMovie  mediaKind = "movie"
	kindSeries mediaKind = "series"
)

// watchedEvent is one Jellyfin "played" notification, stored as it arrives.
type watchedEvent struct {
	ID         int64
	Kind       mediaKind
	ItemID     string // Radarr movie ID or Sonarr series ID, as a string
	Title      string
	User       string
	WatchedAt  time.Time
	ActionedAt *time.Time // set once cleanup has run for this event
}

type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) Close() error {
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS watched_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	kind        TEXT NOT NULL,
	item_id     TEXT NOT NULL,
	title       TEXT NOT NULL,
	user        TEXT NOT NULL,
	watched_at  DATETIME NOT NULL,
	actioned_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_watched_events_pending
	ON watched_events (kind, item_id) WHERE actioned_at IS NULL;
`

// recordWatched inserts a new watched event. Called once per webhook.
func (s *store) recordWatched(e watchedEvent) error {
	_, err := s.db.Exec(
		`INSERT INTO watched_events (kind, item_id, title, user, watched_at) VALUES (?, ?, ?, ?, ?)`,
		e.Kind, e.ItemID, e.Title, e.User, e.WatchedAt,
	)
	return err
}

// duePending returns the earliest not-yet-actioned watched event per
// (kind, item_id) whose watched_at is at or before the cutoff.
func (s *store) duePending(cutoff time.Time) ([]watchedEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, kind, item_id, title, user, watched_at
		FROM watched_events
		WHERE actioned_at IS NULL AND watched_at <= ?
		GROUP BY kind, item_id
		HAVING MIN(watched_at)
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []watchedEvent
	for rows.Next() {
		var e watchedEvent
		if err := rows.Scan(&e.ID, &e.Kind, &e.ItemID, &e.Title, &e.User, &e.WatchedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// markActioned marks every pending event for (kind, item_id) as done, so a
// title already cleaned up doesn't get reprocessed.
func (s *store) markActioned(kind mediaKind, itemID string, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE watched_events SET actioned_at = ? WHERE kind = ? AND item_id = ? AND actioned_at IS NULL`,
		at, kind, itemID,
	)
	return err
}
