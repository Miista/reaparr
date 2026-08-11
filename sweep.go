package main

import (
	"context"
	"log/slog"
	"time"
)

// sweeper periodically checks stored watched events and cleans up anything
// past its grace period. Runs as an internal ticker inside this same
// binary — no external scheduler (e.g. Ofelia) dependency, since this
// service lives in its own compose project separate from the one where
// Ofelia currently runs.
type sweeper struct {
	store       *store
	arr         *arrClient
	gracePeriod time.Duration
	interval    time.Duration
	log         *slog.Logger
}

func (s *sweeper) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.sweepOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce()
		}
	}
}

func (s *sweeper) sweepOnce() {
	cutoff := time.Now().UTC().Add(-s.gracePeriod)
	s.log.Debug("sweep starting", "cutoff", cutoff)

	due, err := s.store.duePending(cutoff)
	if err != nil {
		s.log.Error("failed to query pending events", "error", err)
		return
	}

	if len(due) == 0 {
		s.log.Info("sweep complete", "due", 0, "cleaned", 0, "failed", 0)
		return
	}

	var cleaned, failed int
	for _, e := range due {
		s.log.Debug("evaluating due event", "kind", e.Kind, "item_id", e.ItemID, "title", e.Title, "watched_at", e.WatchedAt)

		var err error
		switch e.Kind {
		case kindMovie:
			err = s.arr.deleteMovie(e.ItemID)
		case kindSeries:
			err = s.arr.deleteSeries(e.ItemID)
		}
		if err != nil {
			s.log.Error("failed to delete via arr API, will retry next sweep", "kind", e.Kind, "item_id", e.ItemID, "title", e.Title, "error", err)
			failed++
			continue
		}

		if err := s.store.markActioned(e.Kind, e.ItemID, time.Now().UTC()); err != nil {
			s.log.Error("deleted but failed to mark actioned, will retry delete next sweep", "kind", e.Kind, "item_id", e.ItemID, "title", e.Title, "error", err)
			failed++
			continue
		}

		s.log.Info("cleaned up watched item", "kind", e.Kind, "item_id", e.ItemID, "title", e.Title)
		cleaned++
	}

	s.log.Info("sweep complete", "due", len(due), "cleaned", cleaned, "failed", failed)
}
