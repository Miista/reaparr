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
// Jellyfin data, rather than remembering anything about a prior sweep.
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
	jellyfin *jellyfinClient
	arr      *arrClient

	// Movies and TV are gated by independent grace periods — a household
	// might want a short one for movies (single-sitting watches) and a
	// longer one for TV (a season pack might sit half-watched between
	// episodes for a while).
	moviesGracePeriod time.Duration
	tvGracePeriod     time.Duration

	schedule cron.Schedule
	log      *slog.Logger
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

	safety := s.checkHardlinkSafety()

	now := time.Now().UTC()
	s.log.Debug("grace periods for this sweep", "movies_grace_period", s.moviesGracePeriod.String(), "tv_grace_period", s.tvGracePeriod.String())

	latestStop, err := s.jellyfin.latestStopEvents()
	if err != nil {
		s.log.Error("could not read jellyfin's activity log this sweep, will try again next time", "error", err)
		return
	}
	s.log.Debug("jellyfin's activity log mentions this many distinct items (played or not, recently stopped or long ago — this is just raw log size, not a decision by itself)", "activity_log_item_count", len(latestStop))

	playedItems, err := s.currentlyPlayedItems()
	if err != nil {
		s.log.Error("could not check jellyfin's current watched state this sweep, will try again next time", "error", err)
		return
	}
	s.log.Debug("found items currently marked played by jellyfin", "count", len(playedItems))

	var due []jellyfinItem
	for _, item := range playedItems {
		stoppedAt, stopped := latestStop[item.ID]
		if !stopped {
			continue
		}

		gracePeriod := s.gracePeriodFor(item)
		if !stoppedAt.Before(now.Add(-gracePeriod)) {
			continue
		}

		s.log.Info("this item is watched and past its grace period, will delete it now", "title", displayTitle(item), "jellyfin_item_id", item.ID, "stopped_playing_at", stoppedAt, "grace_period", gracePeriod.String())
		due = append(due, item)
	}

	if len(due) == 0 {
		s.log.Info("sweep finished: nothing is due for deletion right now", "currently_played_items", len(playedItems), "activity_log_item_count", len(latestStop))
		return
	}

	var cleaned, skipped, failed int
	for _, item := range due {
		resolved, ok, err := s.resolveToArr(item, safety)
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

	s.log.Info("sweep finished", "currently_played_items", len(playedItems), "activity_log_item_count", len(latestStop), "due_for_deletion", len(due), "successfully_deleted", cleaned, "skipped_not_in_arr", skipped, "failed_to_delete", failed)
}

// gracePeriodFor returns the grace period that applies to an item, based on
// its Jellyfin type — movies and TV are independently configurable (see
// config.go). Episodes use the TV grace period even though Sonarr acts at
// the series level; the two settings both exist to express "how long
// should this show be left alone after an episode stops playing."
func (s *sweeper) gracePeriodFor(item jellyfinItem) time.Duration {
	if item.Type == "Episode" {
		return s.tvGracePeriod
	}
	return s.moviesGracePeriod
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

// hardlinkSafety records, per sweep, whether each configured service's
// copyUsingHardlinks setting is enabled — the precondition for Reaparr's
// "downloads copy stays untouched" guarantee (see arrClient.
// radarrUsesHardlinks). Checked once per sweep, not per item, since it's a
// single global setting per service, not something that varies per title.
//
// Radarr and Sonarr are gated independently: a movies-only misconfiguration
// must not also block TV cleanup that's actually safe, and vice versa —
// each service's own sweep proceeds normally as long as ITS setting is
// correct, regardless of the other.
type hardlinkSafety struct {
	radarrSafe bool
	sonarrSafe bool
}

func (s *sweeper) checkHardlinkSafety() hardlinkSafety {
	var safety hardlinkSafety

	if s.arr.hasRadarr() {
		usesHardlinks, err := s.arr.radarrUsesHardlinks()
		if err != nil {
			s.log.Error("could not check radarr's hardlink setting this sweep, treating radarr as unsafe until it can be confirmed", "error", err)
		} else if !usesHardlinks {
			s.log.Error("radarr is NOT configured to use hardlinks (copyUsingHardlinks=false) — deleting a movie would delete the ONLY copy, breaking any active seed and potentially violating private tracker seed-time/ratio rules. Skipping all movie deletions this sweep. Enable 'Use Hardlinks instead of Copy' in Radarr's Media Management settings.")
		} else {
			safety.radarrSafe = true
		}
	}

	if s.arr.hasSonarr() {
		usesHardlinks, err := s.arr.sonarrUsesHardlinks()
		if err != nil {
			s.log.Error("could not check sonarr's hardlink setting this sweep, treating sonarr as unsafe until it can be confirmed", "error", err)
		} else if !usesHardlinks {
			s.log.Error("sonarr is NOT configured to use hardlinks (copyUsingHardlinks=false) — deleting an episode would delete the ONLY copy, breaking any active seed and potentially violating private tracker seed-time/ratio rules. Skipping all episode deletions this sweep. Enable 'Use Hardlinks instead of Copy' in Sonarr's Media Management settings.")
		} else {
			safety.sonarrSafe = true
		}
	}

	return safety
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
// but Radarr/Sonarr doesn't (or isn't safe to act on this sweep) — a real,
// expected case, not a bug.
func (s *sweeper) resolveToArr(item jellyfinItem, safety hardlinkSafety) (resolvedArrItem, bool, error) {
	switch item.Type {
	case "Movie":
		if !s.arr.hasRadarr() {
			s.log.Debug("radarr isn't configured, ignoring this watched movie", "title", item.Name)
			return resolvedArrItem{}, false, nil
		}
		if !safety.radarrSafe {
			s.log.Debug("skipping this watched movie: radarr's hardlink setting isn't safe this sweep", "title", item.Name)
			return resolvedArrItem{}, false, nil
		}
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
		if !s.arr.hasSonarr() {
			s.log.Debug("sonarr isn't configured, ignoring this watched episode", "series", item.SeriesName, "episode", item.Name)
			return resolvedArrItem{}, false, nil
		}
		if !safety.sonarrSafe {
			s.log.Debug("skipping this watched episode: sonarr's hardlink setting isn't safe this sweep", "series", item.SeriesName, "episode", item.Name)
			return resolvedArrItem{}, false, nil
		}
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
