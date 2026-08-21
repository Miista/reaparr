package main

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
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
	seerr    *seerrClient

	// Movies and TV are gated by independent grace periods — a household
	// might want a short one for movies (single-sitting watches) and a
	// longer one for TV (a season pack might sit half-watched between
	// episodes for a while).
	moviesGracePeriod time.Duration
	tvGracePeriod     time.Duration

	schedule cron.Schedule
	log      zerolog.Logger
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
	s.log.Info().Msg("starting a sweep")

	safety := s.checkHardlinkSafety()

	now := time.Now().UTC()
	s.log.Debug().Msg(fmt.Sprintf("grace periods for this sweep: movies=%s, tv=%s", s.moviesGracePeriod, s.tvGracePeriod))

	latestStop, err := s.jellyfin.latestStopEvents()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not read jellyfin's activity log this sweep, will try again next time: %v", err))
		return
	}
	s.log.Debug().Msg(fmt.Sprintf("jellyfin's activity log mentions %d distinct items (played or not, recently stopped or long ago)", len(latestStop)))

	playedItems, err := s.currentlyPlayedItems()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not check jellyfin's current watched state this sweep, will try again next time: %v", err))
		return
	}
	s.log.Debug().Msg(fmt.Sprintf("%d items are currently marked played by jellyfin", len(playedItems)))

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

		s.log.Info().Msg(fmt.Sprintf("'%s' is watched and past its %s grace period (stopped playing %s) — will delete it now", displayTitle(item), gracePeriod, stoppedAt.Format("2006-01-02 15:04")))
		due = append(due, item)
	}

	if len(due) == 0 {
		s.log.Info().Msg(fmt.Sprintf("sweep finished: nothing due for deletion (%d currently played)", len(playedItems)))
		s.cleanUpSeerr()
		return
	}

	var cleaned, skipped, failed int
	for _, item := range due {
		resolved, ok, err := s.resolveToArr(item, safety)
		if err != nil {
			s.log.Error().Msg(fmt.Sprintf("could not look up '%s' in radarr/sonarr, will retry next sweep: %v", displayTitle(item), err))
			failed++
			continue
		}
		if !ok {
			s.log.Warn().Msg(fmt.Sprintf("'%s' is watched and past its grace period, but radarr/sonarr doesn't know about it — nothing to delete, skipping", displayTitle(item)))
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
			s.log.Error().Msg(fmt.Sprintf("failed to delete '%s' (%s id %s), will retry next sweep: %v", resolved.title, resolved.kind, resolved.id, deleteErr))
			failed++
			continue
		}

		s.log.Info().Msg(fmt.Sprintf("deleted '%s' (%s id %s)", resolved.title, resolved.kind, resolved.id))
		cleaned++
	}

	s.log.Info().Msg(fmt.Sprintf("sweep finished: %d due, %d deleted, %d skipped, %d failed", len(due), cleaned, skipped, failed))

	s.cleanUpSeerr()
}

// cleanUpSeerr deletes Seerr media records whose title has already been
// deleted but whose request record was left behind by Seerr's own "Media
// Availability Sync" job — see SEERR_PLAN.md. Entirely independent of the
// Radarr/Sonarr deletion loop above: a query, not a delete-triggered
// action, so it self-heals regardless of what deleted the title or
// whether a previous attempt at this same cleanup failed (see
// SEERR_PLAN.md's "self-healing" reasoning). Failures here are logged but
// never fail the sweep or affect the cleaned/skipped/failed counters
// above — Seerr tidiness is secondary to the Radarr/Sonarr deletion,
// which already succeeded independently by this point.
func (s *sweeper) cleanUpSeerr() {
	if s.seerr == nil || !s.seerr.hasSeerr() {
		return
	}

	stale, err := s.seerr.deletedRequests()
	if err != nil {
		s.log.Warn().Msg(fmt.Sprintf("could not check seerr for stale requests this sweep, will try again next time: %v", err))
		return
	}
	if len(stale) == 0 {
		return
	}

	var cleaned int
	for _, m := range stale {
		if err := s.seerr.deleteMedia(m.mediaID); err != nil {
			s.log.Warn().Msg(fmt.Sprintf("could not delete seerr's stale request for '%s' (media id %d), will retry next sweep: %v", m.title, m.mediaID, err))
			continue
		}
		s.log.Info().Msg(fmt.Sprintf("deleted seerr's stale request for '%s' (media id %d) — its title was already gone", m.title, m.mediaID))
		cleaned++
	}

	s.log.Info().Msg(fmt.Sprintf("seerr cleanup finished: %d stale, %d deleted", len(stale), cleaned))
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
	s.log.Debug().Msg(fmt.Sprintf("found %d jellyfin users to check", len(users)))

	seen := make(map[string]bool)
	var items []jellyfinItem
	for _, u := range users {
		userItems, err := s.jellyfin.playedItems(u.ID)
		if err != nil {
			s.log.Error().Msg(fmt.Sprintf("failed to fetch played items for user '%s', skipping: %v", u.Name, err))
			continue
		}
		s.log.Debug().Msg(fmt.Sprintf("'%s' has %d played items in jellyfin", u.Name, len(userItems)))

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
			s.log.Error().Msg(fmt.Sprintf("could not check radarr's hardlink setting this sweep, treating radarr as unsafe until confirmed: %v", err))
		} else if !usesHardlinks {
			s.log.Error().Msg("radarr is NOT using hardlinks — deleting a movie would delete the only copy and break any active seed. Skipping all movie deletions this sweep. Enable 'Use Hardlinks instead of Copy' in Radarr's Media Management settings.")
		} else {
			safety.radarrSafe = true
		}
	}

	if s.arr.hasSonarr() {
		usesHardlinks, err := s.arr.sonarrUsesHardlinks()
		if err != nil {
			s.log.Error().Msg(fmt.Sprintf("could not check sonarr's hardlink setting this sweep, treating sonarr as unsafe until confirmed: %v", err))
		} else if !usesHardlinks {
			s.log.Error().Msg("sonarr is NOT using hardlinks — deleting an episode would delete the only copy and break any active seed. Skipping all episode deletions this sweep. Enable 'Use Hardlinks instead of Copy' in Sonarr's Media Management settings.")
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
			s.log.Debug().Msg(fmt.Sprintf("radarr isn't configured, ignoring watched movie '%s'", item.Name))
			return resolvedArrItem{}, false, nil
		}
		if !safety.radarrSafe {
			s.log.Debug().Msg(fmt.Sprintf("skipping '%s': radarr's hardlink setting isn't safe this sweep", item.Name))
			return resolvedArrItem{}, false, nil
		}
		if item.ProviderIds.Tmdb == "" {
			s.log.Warn().Msg(fmt.Sprintf("jellyfin has no TMDB id for watched movie '%s', cannot match it to radarr", item.Name))
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
			s.log.Debug().Msg(fmt.Sprintf("sonarr isn't configured, ignoring watched episode '%s' of '%s'", item.Name, item.SeriesName))
			return resolvedArrItem{}, false, nil
		}
		if !safety.sonarrSafe {
			s.log.Debug().Msg(fmt.Sprintf("skipping '%s': sonarr's hardlink setting isn't safe this sweep", item.SeriesName))
			return resolvedArrItem{}, false, nil
		}
		tvdbID, err := s.jellyfin.seriesTvdbID(item.SeriesID)
		if err != nil {
			return resolvedArrItem{}, false, err
		}
		if tvdbID == "" {
			s.log.Warn().Msg(fmt.Sprintf("jellyfin has no TVDB id for '%s' (series of watched episode '%s'), cannot match it to sonarr", item.SeriesName, item.Name))
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
