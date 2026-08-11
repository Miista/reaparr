package main

import (
	"context"
	"log"
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

	due, err := s.store.duePending(cutoff)
	if err != nil {
		log.Printf("sweep: query pending: %v", err)
		return
	}

	for _, e := range due {
		var err error
		switch e.Kind {
		case kindMovie:
			err = s.arr.deleteMovie(e.ItemID)
		case kindSeries:
			err = s.arr.deleteSeries(e.ItemID)
		}
		if err != nil {
			log.Printf("sweep: delete %s %q (id=%s): %v", e.Kind, e.Title, e.ItemID, err)
			continue
		}

		if err := s.store.markActioned(e.Kind, e.ItemID, time.Now().UTC()); err != nil {
			log.Printf("sweep: mark actioned %s %q (id=%s): %v", e.Kind, e.Title, e.ItemID, err)
			continue
		}

		log.Printf("sweep: cleaned up %s %q (id=%s)", e.Kind, e.Title, e.ItemID)
	}
}
