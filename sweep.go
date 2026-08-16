package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// mediaKind distinguishes which *arr API owns an item.
type mediaKind string

const (
	kindMovie  mediaKind = "movie"
	kindSeries mediaKind = "series"
)

// sweeper runs on a cron schedule. Reaparr is entirely stateless: every
// sweep independently re-derives everything it needs to know from live
// Jellyfin data, rather than remembering anything about a prior sweep. See
// plan.md's Architecture section for the full history of how this design
// was reached — it replaced an earlier webhook-listener design, then a
// SQLite-backed store, then a per-item watched-state store, before landing
// here.
//
// A sweep does three things:
//  1. Ask Jellyfin's Activity Log for every item with a VideoPlaybackStopped
//     event older than the grace period (set A).
//  2. Ask Jellyfin for every item every user currently has Played=true (set B).
//  3. For each item in A ∩ B, resolve it to Radarr/Sonarr's own ID (via
//     TMDB/TVDB) and delete it.
//
// The intersection is what makes this safe: A alone doesn't mean
// "finished" (VideoPlaybackStopped fires on any stop, including someone
// quitting partway through), and B alone doesn't tell you when. Only items
// that are BOTH currently fully played AND stopped playing a while ago are
// acted on. Because B is re-checked live every sweep, a title that gets
// unplayed again (started, abandoned, Played flips back to false) simply
// stops appearing in B and is never touched — there is nothing stored to
// go stale.
//
// Restarting the container is a safe, complete reset: with no persisted
// state, a fresh process performs a fresh, fully-correct sweep immediately
// on startup.
type sweeper struct {
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
	s.log.Info("starting a sweep: checking jellyfin's activity log and current watched state, then deleting anything eligible")

	cutoff := time.Now().UTC().Add(-s.gracePeriod)
	s.log.Debug("looking for playback-stopped events older than this cutoff", "cutoff", cutoff, "grace_period", s.gracePeriod.String())

	stoppedBefore, err := s.jellyfin.itemsStoppedBefore(cutoff)
	if err != nil {
		s.log.Error("could not read jellyfin's activity log this sweep, will try again next time", "error", err)
		return
	}
	s.log.Debug("found items with an old-enough playback-stopped event", "count", len(stoppedBefore))

	playedItems, err := s.currentlyPlayedItems()
	if err != nil {
		s.log.Error("could not check jellyfin's current watched state this sweep, will try again next time", "error", err)
		return
	}
	s.log.Debug("found items currently marked played by jellyfin", "count", len(playedItems))

	var due []jellyfinItem
	for _, item := range playedItems {
		stoppedAt, stopped := stoppedBefore[item.ID]
		if !stopped {
			continue
		}
		s.log.Info("this item is watched and past its grace period, will delete it now", "title", displayTitle(item), "jellyfin_item_id", item.ID, "stopped_playing_at", stoppedAt, "grace_period", s.gracePeriod.String())
		due = append(due, item)
	}

	if len(due) == 0 {
		s.log.Info("sweep finished: nothing is due for deletion right now", "currently_played_items", len(playedItems), "items_with_old_stop_event", len(stoppedBefore))
		return
	}

	var cleaned, skipped, failed int
	for _, item := range due {
		resolved, ok, err := s.resolveToArr(item)
		if err != nil {
			s.log.Error("could not look this item up in radarr/sonarr, will retry next sweep", "title", displayTitle(item), "error", err)
			failed++
			continue
		}
		if !ok {
			s.log.Warn("jellyfin has this item watched and past its grace period, but radarr/sonarr doesn't know about it — nothing to delete, skipping", "title", displayTitle(item), "jellyfin_item_id", item.ID)
			skipped++
			continue
		}

		var deleteErr error
		switch resolved.kind {
		case kindMovie:
			deleteErr = s.arr.deleteMovie(resolved.id)
		case kindSeries:
			deleteErr = s.arr.deleteSeries(resolved.id)
		}
		if deleteErr != nil {
			s.log.Error("delete failed, will try again next sweep — nothing was removed", "kind", resolved.kind, "item_id", resolved.id, "title", resolved.title, "error", deleteErr)
			failed++
			continue
		}

		s.log.Info("successfully deleted watched item", "kind", resolved.kind, "title", resolved.title, "item_id", resolved.id)
		cleaned++
	}

	s.log.Info("sweep finished", "currently_played_items", len(playedItems), "items_with_old_stop_event", len(stoppedBefore), "due_for_deletion", len(due), "successfully_deleted", cleaned, "skipped_not_in_arr", skipped, "failed_to_delete", failed)
}

// currentlyPlayedItems returns every movie/episode any user currently has
// marked played. Always live — see the sweeper doc comment on why nothing
// here is cached across sweeps.
func (s *sweeper) currentlyPlayedItems() ([]jellyfinItem, error) {
	users, err := s.jellyfin.users()
	if err != nil {
		return nil, err
	}
	s.log.Debug("found jellyfin users to check", "user_count", len(users))

	seen := make(map[string]bool)
	var items []jellyfinItem
	for _, u := range users {
		userItems, err := s.jellyfin.playedItems(u.ID)
		if err != nil {
			s.log.Error("failed to fetch played items for user, skipping", "user", u.Name, "error", err)
			continue
		}
		s.log.Debug("checked user's played items in jellyfin", "user", u.Name, "played_item_count", len(userItems))

		for _, item := range userItems {
			if seen[item.ID] {
				continue // another user already has this played; either is sufficient
			}
			seen[item.ID] = true
			items = append(items, item)
		}
	}
	return items, nil
}

// resolvedArrItem is a played Jellyfin item successfully matched to its
// Radarr/Sonarr counterpart, ready to delete.
type resolvedArrItem struct {
	kind  mediaKind
	id    string
	title string
}

// resolveToArr maps a played Jellyfin item to Radarr/Sonarr's own internal
// ID, via TMDB (movies) or TVDB (series, via a series-level lookup for
// episodes — see jellyfinClient.seriesTvdbID). Jellyfin's own item ID means
// nothing to Radarr/Sonarr's delete APIs, so this lookup is required, not
// optional. ok=false (with no error) means Jellyfin knows about this item
// but Radarr/Sonarr doesn't — a real, expected case, not a bug.
func (s *sweeper) resolveToArr(item jellyfinItem) (resolvedArrItem, bool, error) {
	switch item.Type {
	case "Movie":
		if item.ProviderIds.Tmdb == "" {
			s.log.Warn("jellyfin has no TMDB id for this watched movie, cannot match it to radarr", "title", item.Name, "jellyfin_item_id", item.ID)
			return resolvedArrItem{}, false, nil
		}
		movie, ok, err := s.arr.findMovieByTmdbID(item.ProviderIds.Tmdb)
		if err != nil {
			return resolvedArrItem{}, false, err
		}
		if !ok {
			return resolvedArrItem{}, false, nil
		}
		return resolvedArrItem{kind: kindMovie, id: fmt.Sprint(movie.ID), title: movie.Title}, true, nil

	case "Episode":
		tvdbID, err := s.jellyfin.seriesTvdbID(item.SeriesID)
		if err != nil {
			return resolvedArrItem{}, false, err
		}
		if tvdbID == "" {
			s.log.Warn("jellyfin has no TVDB id for this watched episode's series, cannot match it to sonarr", "series", item.SeriesName, "episode", item.Name)
			return resolvedArrItem{}, false, nil
		}
		series, ok, err := s.arr.findSeriesByTvdbID(tvdbID)
		if err != nil {
			return resolvedArrItem{}, false, err
		}
		if !ok {
			return resolvedArrItem{}, false, nil
		}
		return resolvedArrItem{kind: kindSeries, id: fmt.Sprint(series.ID), title: series.Title}, true, nil

	default:
		return resolvedArrItem{}, false, nil
	}
}

func displayTitle(item jellyfinItem) string {
	if item.Type == "Episode" {
		return item.SeriesName
	}
	return item.Name
}
