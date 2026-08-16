package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// sweeper runs on a cron schedule, doing two things each run: polling
// Jellyfin for currently-played items (recording/refreshing their watched
// state), then cleaning up anything past its grace period via
// Radarr/Sonarr. Polling replaces the earlier webhook-listener design —
// see plan.md's Architecture section — so there is no
// separate always-on HTTP listener; this is the entire program.
type sweeper struct {
	store       *store
	jellyfin    *jellyfinClient
	arr         *arrClient
	gracePeriod time.Duration
	schedule    cron.Schedule
	log         *slog.Logger
}

func (s *sweeper) run(ctx context.Context) {
	s.sweepOnce()

	now := time.Now()
	next := s.schedule.Next(now)
	timer := time.NewTimer(next.Sub(now))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			s.sweepOnce()
			next := s.schedule.Next(now)
			timer.Reset(time.Until(next))
		}
	}
}

func (s *sweeper) sweepOnce() {
	s.log.Debug("sweep starting")

	polled, err := s.pollJellyfin()
	if err != nil {
		s.log.Error("failed to poll jellyfin, will retry next sweep", "error", err)
		// Deletion still proceeds below against whatever was already
		// recorded from prior successful polls — a transient Jellyfin
		// outage shouldn't also block cleanup of already-known items.
	}

	cutoff := time.Now().UTC().Add(-s.gracePeriod)
	due, err := s.store.duePending(cutoff)
	if err != nil {
		s.log.Error("failed to query pending items", "error", err)
		return
	}

	if len(due) == 0 {
		s.log.Info("sweep complete", "polled", polled, "due", 0, "cleaned", 0, "failed", 0)
		return
	}

	var cleaned, failed int
	for _, it := range due {
		s.log.Debug("evaluating due item", "kind", it.Kind, "item_id", it.ItemID, "title", it.Title, "watched_at", it.WatchedAt)

		var err error
		switch it.Kind {
		case kindMovie:
			err = s.arr.deleteMovie(it.ItemID)
		case kindSeries:
			err = s.arr.deleteSeries(it.ItemID)
		}
		if err != nil {
			s.log.Error("failed to delete via arr API, will retry next sweep", "kind", it.Kind, "item_id", it.ItemID, "title", it.Title, "error", err)
			failed++
			continue
		}

		if err := s.store.markActioned(it.Kind, it.ItemID, time.Now().UTC()); err != nil {
			s.log.Error("deleted but failed to mark actioned, will retry delete next sweep", "kind", it.Kind, "item_id", it.ItemID, "title", it.Title, "error", err)
			failed++
			continue
		}

		s.log.Info("cleaned up watched item", "kind", it.Kind, "item_id", it.ItemID, "title", it.Title)
		cleaned++
	}

	s.log.Info("sweep complete", "polled", polled, "due", len(due), "cleaned", cleaned, "failed", failed)
}

// pollJellyfin queries every user's played items and upserts them into the
// store. Returns the number of played items observed across all users.
func (s *sweeper) pollJellyfin() (int, error) {
	users, err := s.jellyfin.users()
	if err != nil {
		return 0, err
	}

	var total int
	for _, u := range users {
		items, err := s.jellyfin.playedItems(u.ID)
		if err != nil {
			s.log.Error("failed to fetch played items for user, skipping", "user", u.Name, "error", err)
			continue
		}

		for _, item := range items {
			w, ok := watchedItemFromJellyfin(item, u.Name, s.log)
			if !ok {
				continue
			}
			if err := s.store.upsertWatched(w); err != nil {
				s.log.Error("failed to upsert watched item", "error", err, "kind", w.Kind, "item_id", w.ItemID, "title", w.Title)
				continue
			}
			total++
		}
	}

	return total, nil
}

// watchedItemFromJellyfin maps a played Jellyfin item to a watchedItem.
// Movies use their own ID; episodes use their series' ID, since Sonarr
// tracks/deletes at the series level per the season-pack scope decision.
func watchedItemFromJellyfin(item jellyfinItem, user string, log *slog.Logger) (watchedItem, bool) {
	var kind mediaKind
	var itemID, title string

	switch item.Type {
	case "Movie":
		kind, itemID, title = kindMovie, item.ID, item.Name
	case "Episode":
		kind, itemID, title = kindSeries, item.SeriesID, item.SeriesName
	default:
		return watchedItem{}, false
	}

	return watchedItem{
		Kind:      kind,
		ItemID:    itemID,
		Title:     title,
		User:      user,
		WatchedAt: parseLastPlayedDate(item.UserData.LastPlayedDate, log),
	}, true
}
