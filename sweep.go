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
	kindSeason mediaKind = "season" // one season of a series — see seasons.go
)

// sweeper runs on a cron schedule. Reaparr is entirely stateless: every
// sweep independently re-derives everything it needs to know from live
// Jellyfin data, rather than remembering anything about a prior sweep.
//
// A sweep does three things:
//  1. Ask Jellyfin's Activity Log for every item with a VideoPlaybackStopped
//     event older than the grace period (set A).
//  2. Ask Jellyfin for every item every user currently has Played=true (set B).
//  3. For each movie in A ∩ B, resolve it to Radarr's own ID (via TMDB) and
//     delete it. Episodes are grouped into seasons and a season is only
//     deleted once it is entirely watched — see seasons.go.
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
	// daemonEnabled gates only the scheduled sweep (see liveSweeper.sweep);
	// the preview and the dashboard's delete buttons work either way.
	daemonEnabled bool
	log           zerolog.Logger
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

// deleteAllDue runs the delete pass over an already-computed due list,
// returning how many items were deleted, skipped (no radarr/sonarr match)
// and failed.
func (s *sweeper) deleteAllDue(due []dueItem) (cleaned, skipped, failed int) {
	if len(due) == 0 {
		s.log.Info().Msg("sweep finished: nothing due for deletion")
		s.cleanUpSeerr()
		return 0, 0, 0
	}

	for _, d := range due {
		if !d.resolved {
			s.log.Warn().Msg(fmt.Sprintf("'%s' is watched, but can't be deleted (%s) — skipping", d.title, d.reason))
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
	return cleaned, skipped, failed
}

// dueItem is one watched movie or one fully watched season, together with
// whatever Radarr/Sonarr resolution was possible for it. due=false means
// it's still within its grace period (shown on the dashboard as waiting,
// never deleted). resolved=false means it could not be matched into
// Radarr/Sonarr (hardlink-unsafe, a failed lookup, or simply untracked
// there) — reason says which, for the log and the dashboard.
type dueItem struct {
	id          string // Jellyfin item ID for a movie; seasonItemID for a season
	title       string
	kind        mediaKind
	gracePeriod time.Duration
	stoppedAt   time.Time
	due         bool // past its grace period
	resolved    bool
	reason      string        // set when resolved=false
	movie       radarrMovie   // kind=movie, resolved
	season      *seasonTarget // kind=season, resolved
}

// findDue returns only what is past its grace period — what the scheduled
// sweep acts on. Manual deletes use findCandidates (no grace period).
func (s *sweeper) findDue() ([]dueItem, error) {
	candidates, err := s.findCandidates()
	if err != nil {
		return nil, err
	}
	var due []dueItem
	for _, d := range candidates {
		if d.due {
			due = append(due, d)
		}
	}
	return due, nil
}

// findCandidates performs the full matching computation — the intersection
// of currently-played items (set B) and their stop events (set A), each
// resolved against Radarr/Sonarr — without deleting anything. It returns
// every watched movie and fully watched season, whether or not its grace
// period has passed (due says which), so the dashboard can show what's
// waiting as well as what's due. This is the single source of truth: the
// cron sweep, the dashboard preview, and a re-verify before a manual delete
// all go through it, never two algorithms that could silently disagree.
func (s *sweeper) findCandidates() ([]dueItem, error) {
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
		if item.Type != "Movie" {
			continue
		}
		stoppedAt, stopped := latestStop[item.ID]
		if d, ok := s.evaluateMovie(item, stoppedAt, stopped, now, safety); ok {
			due = append(due, d)
		}
	}

	lookups := newSonarrLookups(s)
	for _, g := range s.groupPlayedEpisodes(playedItems, latestStop) {
		if d, ok := s.evaluateSeason(g, now, safety, lookups); ok {
			due = append(due, d)
		}
	}

	return due, nil
}

// findOneCandidate re-verifies, right now, whether a single movie or
// season is still a deletion candidate — used by a manual delete request so
// it acts on freshly-confirmed reality rather than a possibly-stale cached
// list the dashboard is displaying (see liveSweeper.cachedDue in
// adapter.go). The grace period does NOT apply here: it only governs the
// scheduled sweep, while a manual delete may act on anything watched. It
// runs the full findCandidates and picks the requested row, so there is
// only one matching algorithm — a season's eligibility depends on all its
// episodes, so it can't be re-checked in isolation anyway.
//
// ok=false means the item is no longer a candidate — it got unplayed (or a
// season is no longer fully watched) since the cached list was built. That
// is reported to the caller as "nothing to delete", not as an error: a
// cache that's gone stale in the user's favor is the whole point of
// re-verifying.
func (s *sweeper) findOneCandidate(id string) (dueItem, bool, error) {
	candidates, err := s.findCandidates()
	if err != nil {
		return dueItem{}, false, err
	}
	for _, d := range candidates {
		if d.id == id {
			return d, true, nil
		}
	}
	return dueItem{}, false, nil
}

// evaluateMovie is the per-movie core of findCandidates' matching
// algorithm. ok=false means this movie isn't a candidate at all (no stop
// event yet, or Radarr isn't configured); a movie still within its grace
// period is a candidate with due=false.
func (s *sweeper) evaluateMovie(item jellyfinItem, stoppedAt time.Time, stopped bool, now time.Time, safety hardlinkSafety) (dueItem, bool) {
	if !stopped {
		s.log.Debug().Msg(fmt.Sprintf("'%s' is played but has no stop event in jellyfin's activity log, skipping", item.Name))
		return dueItem{}, false
	}

	if !s.arr.hasRadarr() {
		s.log.Debug().Msg(fmt.Sprintf("radarr isn't configured, ignoring watched movie '%s'", item.Name))
		return dueItem{}, false
	}

	gracePeriod := s.gracePeriodFor(item)
	d := dueItem{id: item.ID, title: item.Name, kind: kindMovie, gracePeriod: gracePeriod, stoppedAt: stoppedAt}
	d.due = stoppedAt.Before(now.Add(-gracePeriod))
	if d.due {
		s.log.Info().Msg(fmt.Sprintf("'%s' is watched and past its %s grace period (stopped playing %s)", item.Name, gracePeriod, stoppedAt.Local().Format("2006-01-02 15:04")))
	} else {
		s.log.Debug().Msg(fmt.Sprintf("'%s' stopped playing %s, still within its %s grace period", item.Name, stoppedAt.Local().Format("2006-01-02 15:04"), gracePeriod))
	}

	movie, reason := s.resolveMovie(item, safety)
	if reason != "" {
		if d.due {
			s.log.Warn().Msg(fmt.Sprintf("'%s' can't be matched to radarr: %s", item.Name, reason))
		}
		d.reason = reason
		return d, true
	}
	d.resolved = true
	d.movie = movie
	return d, true
}

// deleteDueItem deletes one item previously returned by findDue: a movie
// via Radarr, a season via Sonarr (see deleteSeason). This is also what the
// HTTP API's manual delete actions call, so there is only ever one delete
// code path.
func (s *sweeper) deleteDueItem(d dueItem) error {
	var deleteErr error
	switch d.kind {
	case kindMovie:
		deleteErr = s.arr.deleteMovie(fmt.Sprint(d.movie.ID))
	case kindSeason:
		deleteErr = s.deleteSeason(d.season)
	default:
		deleteErr = fmt.Errorf("unknown kind %q", d.kind)
	}
	if deleteErr != nil {
		s.log.Error().Msg(fmt.Sprintf("failed to delete '%s', will retry next sweep: %v", d.title, deleteErr))
		return deleteErr
	}

	s.log.Info().Msg(fmt.Sprintf("deleted '%s'", d.title))
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

// resolveMovie maps a played Jellyfin movie to Radarr's own internal ID via
// its TMDB ID — Jellyfin's item ID means nothing to Radarr's delete API.
// A non-empty reason means it can't be acted on (hardlink-unsafe, a failed
// lookup, or untracked in Radarr) — a real, expected case, not a bug.
func (s *sweeper) resolveMovie(item jellyfinItem, safety hardlinkSafety) (radarrMovie, string) {
	if !safety.radarrSafe {
		return radarrMovie{}, "Radarr's hardlink setting isn't confirmed safe"
	}
	if item.ProviderIds.Tmdb == "" {
		return radarrMovie{}, "Jellyfin has no TMDB id for this movie"
	}
	movie, ok, err := s.arr.findMovieByTmdbID(item.ProviderIds.Tmdb)
	if err != nil {
		return radarrMovie{}, fmt.Sprintf("Radarr lookup failed: %v", err)
	}
	if !ok {
		return radarrMovie{}, "Not found in Radarr"
	}
	return movie, ""
}

func displayTitle(item jellyfinItem) string {
	if item.Type == "Episode" {
		return item.SeriesName
	}
	return item.Name
}
