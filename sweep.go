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

	due, err := s.findDue()
	if err != nil {
		// findDue already logged the specific cause.
		return
	}

	s.deleteAllDue(due)
}

// missingServices lists the required services that aren't configured
// (both URL and API key set): Jellyfin, Radarr or Sonarr, and Seerr. A
// non-empty result means the sweep must not run — see liveSweeper.sweep.
func (s *sweeper) missingServices() []string {
	var missing []string
	if s.jellyfin.baseURL == "" || s.jellyfin.apiKey == "" {
		missing = append(missing, "Jellyfin")
	}
	radarr := s.arr.radarrURL != "" && s.arr.radarrAPIKey != ""
	sonarr := s.arr.sonarrURL != "" && s.arr.sonarrAPIKey != ""
	if !radarr && !sonarr {
		missing = append(missing, "Radarr or Sonarr")
	}
	if s.seerr.baseURL == "" || s.seerr.apiKey == "" {
		missing = append(missing, "Seerr")
	}
	return missing
}

// deleteAllDue runs the delete pass over an already-computed due list.
func (s *sweeper) deleteAllDue(due []dueItem) {
	if len(due) == 0 {
		s.log.Info().Msg("sweep finished: nothing due for deletion")
		s.cleanUpSeerr()
		return
	}

	var cleaned, skipped, failed int
	for _, d := range due {
		if !d.resolved {
			s.log.Warn().Msg(fmt.Sprintf("'%s' is watched and past its grace period, but radarr/sonarr doesn't know about it — nothing to delete, skipping", displayTitle(d.item)))
			skipped++
			continue
		}

		if err := s.deleteDueItem(d); err != nil {
			failed++
			continue
		}
		cleaned++
	}

	s.log.Info().Msg(fmt.Sprintf("sweep finished: %d due, %d deleted, %d skipped, %d failed", len(due), cleaned, skipped, failed))

	s.cleanUpSeerr()
}

// dueItem is one Jellyfin item that is both currently played and past its
// grace period, together with whatever Radarr/Sonarr resolution was
// possible for it. resolved=false means Jellyfin knows about this item but
// it could not be matched into Radarr/Sonarr (not configured,
// hardlink-unsafe, or simply untracked there) — a real, expected case, not
// an error; see resolveToArr's doc comment.
type dueItem struct {
	item        jellyfinItem
	gracePeriod time.Duration
	stoppedAt   time.Time
	resolved    bool
	arrItem     resolvedArrItem
}

// findDue performs the full "what's due for deletion" computation — the
// intersection of currently-played items (set B) and old-enough stop events
// (set A), each resolved against Radarr/Sonarr — without deleting anything.
// This is the single source of truth for "what is due": the cron sweep
// (sweepOnce), the HTTP preview endpoint, and a single-item re-verify
// before a manual delete (see evaluateItem, reused by all three) share this
// one matching algorithm, never two that could silently disagree.
func (s *sweeper) findDue() ([]dueItem, error) {
	safety := s.checkHardlinkSafety()

	latestStop, err := s.jellyfin.latestStopEvents()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not read jellyfin's activity log this sweep, will try again next time: %v", err))
		return nil, err
	}
	s.log.Debug().Msg(fmt.Sprintf("jellyfin's activity log mentions %d distinct items (played or not, recently stopped or long ago)", len(latestStop)))

	playedItems, err := s.currentlyPlayedItems()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not check jellyfin's current watched state this sweep, will try again next time: %v", err))
		return nil, err
	}
	s.log.Debug().Msg(fmt.Sprintf("%d items are currently marked played by jellyfin", len(playedItems)))

	now := time.Now().UTC()
	s.log.Debug().Msg(fmt.Sprintf("grace periods for this sweep: movies=%s, tv=%s", s.moviesGracePeriod, s.tvGracePeriod))

	var due []dueItem
	for _, item := range playedItems {
		stoppedAt, stopped := latestStop[item.ID]
		if d, ok := s.evaluateItem(item, stoppedAt, stopped, now, safety); ok {
			due = append(due, d)
		}
	}

	return due, nil
}

// findOneDue re-verifies, right now, whether a single specific Jellyfin
// item is currently due for deletion — used by a manual delete request so
// it acts on freshly-confirmed reality rather than a possibly-stale cached
// list the dashboard is displaying (see liveSweeper.cachedDue in
// adapter.go). Calls the exact same evaluateItem helper findDue uses, just
// scoped to one item instead of every currently-played one, so there is
// still only one matching algorithm, not a second one for this path.
//
// ok=false means the item is not currently due — either it was never
// played, got unplayed since the cached list was built, or is still within
// its grace period. That is reported to the caller as "nothing to delete",
// not as an error: a cache that's gone stale in the user's favor (the item
// no longer qualifies) is the whole point of re-verifying, not a bug.
func (s *sweeper) findOneDue(jellyfinItemID string) (dueItem, bool, error) {
	safety := s.checkHardlinkSafety()

	latestStop, err := s.jellyfin.latestStopEvents()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not read jellyfin's activity log for a manual delete re-check, aborting: %v", err))
		return dueItem{}, false, err
	}

	playedItems, err := s.currentlyPlayedItems()
	if err != nil {
		s.log.Error().Msg(fmt.Sprintf("could not check jellyfin's current watched state for a manual delete re-check, aborting: %v", err))
		return dueItem{}, false, err
	}

	now := time.Now().UTC()
	for _, item := range playedItems {
		if item.ID != jellyfinItemID {
			continue
		}
		stoppedAt, stopped := latestStop[item.ID]
		d, ok := s.evaluateItem(item, stoppedAt, stopped, now, safety)
		return d, ok, nil
	}

	return dueItem{}, false, nil
}

// evaluateItem is the per-item core of findDue's matching algorithm,
// extracted so findOneDue can re-run it for a single item without
// duplicating the logic by hand. ok=false means this item doesn't belong
// in the due set right now (never played, no stop event yet, or still
// within its grace period) — logged at debug level since it's the common,
// expected case for most of the library, not a problem.
func (s *sweeper) evaluateItem(item jellyfinItem, stoppedAt time.Time, stopped bool, now time.Time, safety hardlinkSafety) (dueItem, bool) {
	if !stopped {
		s.log.Debug().Msg(fmt.Sprintf("'%s' is played but has no stop event in jellyfin's activity log, skipping", displayTitle(item)))
		return dueItem{}, false
	}

	gracePeriod := s.gracePeriodFor(item)
	if !stoppedAt.Before(now.Add(-gracePeriod)) {
		s.log.Debug().Msg(fmt.Sprintf("'%s' stopped playing %s, still within its %s grace period", displayTitle(item), stoppedAt.Local().Format("2006-01-02 15:04"), gracePeriod))
		return dueItem{}, false
	}

	s.log.Info().Msg(fmt.Sprintf("'%s' is watched and past its %s grace period (stopped playing %s)", displayTitle(item), gracePeriod, stoppedAt.Local().Format("2006-01-02 15:04")))

	d := dueItem{item: item, gracePeriod: gracePeriod, stoppedAt: stoppedAt}
	resolved, ok, resolveErr := s.resolveToArr(item, safety)
	if resolveErr != nil {
		s.log.Error().Msg(fmt.Sprintf("could not look up '%s' in radarr/sonarr, will retry next sweep: %v", displayTitle(item), resolveErr))
		// Still reported as due (so the UI preview can show it), just
		// unresolved — a manual delete attempt will surface the same
		// error.
	} else if ok {
		d.resolved = true
		d.arrItem = resolved
	}
	return d, true
}

// deleteDueItem deletes one item previously returned by findDue, via the
// same arrClient.deleteMovie/deleteSeries calls the cron sweep has always
// used. This is also what the HTTP API's manual single-item delete action
// calls, so there is only ever one delete code path.
func (s *sweeper) deleteDueItem(d dueItem) error {
	var deleteErr error
	switch d.arrItem.kind {
	case kindMovie:
		deleteErr = s.arr.deleteMovie(d.arrItem.id)
	case kindSeries:
		deleteErr = s.arr.deleteSeries(d.arrItem.id)
	}
	if deleteErr != nil {
		s.log.Error().Msg(fmt.Sprintf("failed to delete '%s' (%s id %s), will retry next sweep: %v", d.arrItem.title, d.arrItem.kind, d.arrItem.id, deleteErr))
		return deleteErr
	}

	s.log.Info().Msg(fmt.Sprintf("deleted '%s' (%s id %s)", d.arrItem.title, d.arrItem.kind, d.arrItem.id))
	return nil
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
			s.log.Debug().Msg(fmt.Sprintf("'%s' played: '%s' (id %s)", u.Name, displayTitle(item), item.ID))
		}

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
