package main

import (
	"fmt"
	"sort"
	"time"
)

// TV is judged per season, never per episode: a season becomes eligible for
// deletion only once EVERY episode Sonarr tracks for it has aired, has a
// file, and is played in Jellyfin (by any user — see currentlyPlayedItems),
// and the most recent stop event across those episodes is past the TV grace
// period. A season still airing, or with an episode Sonarr hasn't
// downloaded, is never eligible. Deleting a season removes its episode
// files through Sonarr and unmonitors it so it isn't downloaded again; the
// series and its other seasons are left alone.

// seasonGroup collects the played Jellyfin episodes of one season.
type seasonGroup struct {
	jellyfinSeriesID string
	seriesName       string
	season           int
	played           map[int]bool // episode numbers played in Jellyfin
	stoppedAt        time.Time    // latest stop event across the season's episodes
	stopped          bool
}

// seasonItemID is the dashboard/API identifier for a season row — a season
// has no single Jellyfin item ID of its own.
func seasonItemID(jellyfinSeriesID string, season int) string {
	return fmt.Sprintf("season:%s:%d", jellyfinSeriesID, season)
}

// groupPlayedEpisodes buckets played episodes by series and season, sorted
// by series name then season for a stable dashboard order.
func (s *sweeper) groupPlayedEpisodes(items []jellyfinItem, latestStop map[string]time.Time) []*seasonGroup {
	groups := map[string]*seasonGroup{}
	for _, item := range items {
		if item.Type != "Episode" {
			continue
		}
		if item.ParentIndexNumber == nil || item.IndexNumber == nil {
			s.log.Debug().Msg(fmt.Sprintf("'%s' episode '%s' has no season/episode number in jellyfin, skipping", item.SeriesName, item.Name))
			continue
		}
		id := seasonItemID(item.SeriesID, *item.ParentIndexNumber)
		g, ok := groups[id]
		if !ok {
			g = &seasonGroup{
				jellyfinSeriesID: item.SeriesID,
				seriesName:       item.SeriesName,
				season:           *item.ParentIndexNumber,
				played:           map[int]bool{},
			}
			groups[id] = g
		}

		last := *item.IndexNumber
		if item.IndexNumberEnd != nil && *item.IndexNumberEnd > last {
			last = *item.IndexNumberEnd // multi-episode file
		}
		for ep := *item.IndexNumber; ep <= last; ep++ {
			g.played[ep] = true
		}

		if stoppedAt, ok := latestStop[item.ID]; ok && (!g.stopped || stoppedAt.After(g.stoppedAt)) {
			g.stoppedAt = stoppedAt
			g.stopped = true
		}
	}

	out := make([]*seasonGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].seriesName != out[j].seriesName {
			return out[i].seriesName < out[j].seriesName
		}
		return out[i].season < out[j].season
	})
	return out
}

// sonarrLookups memoizes the per-series lookups needed to evaluate seasons
// within a single findDue, so several seasons of the same series don't
// refetch the same data.
type sonarrLookups struct {
	s        *sweeper
	tvdbIDs  map[string]string
	episodes map[int][]sonarrEpisode
}

func newSonarrLookups(s *sweeper) *sonarrLookups {
	return &sonarrLookups{s: s, tvdbIDs: map[string]string{}, episodes: map[int][]sonarrEpisode{}}
}

func (l *sonarrLookups) tvdbID(jellyfinSeriesID string) (string, error) {
	if id, ok := l.tvdbIDs[jellyfinSeriesID]; ok {
		return id, nil
	}
	id, err := l.s.jellyfin.seriesTvdbID(jellyfinSeriesID)
	if err != nil {
		return "", err
	}
	l.tvdbIDs[jellyfinSeriesID] = id
	return id, nil
}

func (l *sonarrLookups) seasonEpisodes(seriesID, season int) ([]sonarrEpisode, error) {
	all, ok := l.episodes[seriesID]
	if !ok {
		var err error
		if all, err = l.s.arr.sonarrEpisodes(seriesID); err != nil {
			return nil, err
		}
		l.episodes[seriesID] = all
	}
	var out []sonarrEpisode
	for _, ep := range all {
		if ep.SeasonNumber == season {
			out = append(out, ep)
		}
	}
	return out, nil
}

// evaluateSeason decides whether one season is ripe for deletion. ok=false
// means it doesn't belong in the due set (not fully watched/aired/
// downloaded, within its grace period, or Sonarr isn't configured at all).
// A row that IS reported but can't be acted on carries resolved=false and a
// reason (unsafe hardlinks, a lookup failure, or untracked in Sonarr).
func (s *sweeper) evaluateSeason(g *seasonGroup, now time.Time, safety hardlinkSafety, lookups *sonarrLookups) (dueItem, bool) {
	title := fmt.Sprintf("%s — Season %d", g.seriesName, g.season)

	if !s.arr.hasSonarr() {
		s.log.Debug().Msg(fmt.Sprintf("sonarr isn't configured, ignoring '%s'", title))
		return dueItem{}, false
	}
	if !g.stopped {
		s.log.Debug().Msg(fmt.Sprintf("'%s' has played episodes but no stop event in jellyfin's activity log, skipping", title))
		return dueItem{}, false
	}
	if !g.stoppedAt.Before(now.Add(-s.tvGracePeriod)) {
		s.log.Debug().Msg(fmt.Sprintf("'%s' last stopped playing %s, still within its %s grace period", title, g.stoppedAt.Local().Format("2006-01-02 15:04"), s.tvGracePeriod))
		return dueItem{}, false
	}

	d := dueItem{
		id:          seasonItemID(g.jellyfinSeriesID, g.season),
		title:       title,
		kind:        kindSeason,
		gracePeriod: s.tvGracePeriod,
		stoppedAt:   g.stoppedAt,
	}
	unresolved := func(reason string) (dueItem, bool) {
		s.log.Warn().Msg(fmt.Sprintf("'%s' can't be checked against sonarr: %s", title, reason))
		d.reason = reason
		return d, true
	}

	if !safety.sonarrSafe {
		return unresolved("Sonarr's hardlink setting isn't confirmed safe")
	}
	tvdbID, err := lookups.tvdbID(g.jellyfinSeriesID)
	if err != nil {
		return unresolved(fmt.Sprintf("Jellyfin series lookup failed: %v", err))
	}
	if tvdbID == "" {
		return unresolved("Jellyfin has no TVDB id for this series")
	}
	series, found, err := s.arr.findSeriesByTvdbID(tvdbID)
	if err != nil {
		return unresolved(fmt.Sprintf("Sonarr series lookup failed: %v", err))
	}
	if !found {
		return unresolved("Series not found in Sonarr")
	}
	episodes, err := lookups.seasonEpisodes(series.ID, g.season)
	if err != nil {
		return unresolved(fmt.Sprintf("Sonarr episode lookup failed: %v", err))
	}
	if len(episodes) == 0 {
		return unresolved("Season not found in Sonarr")
	}

	target := &seasonTarget{seriesID: series.ID, seasonNumber: g.season}
	files := map[int]bool{}
	for _, ep := range episodes {
		switch {
		case ep.AirDateUtc == nil || ep.AirDateUtc.After(now):
			s.log.Debug().Msg(fmt.Sprintf("'%s' isn't eligible: episode %d hasn't aired yet", title, ep.EpisodeNumber))
			return dueItem{}, false
		case !ep.HasFile:
			s.log.Debug().Msg(fmt.Sprintf("'%s' isn't eligible: episode %d has no file in sonarr", title, ep.EpisodeNumber))
			return dueItem{}, false
		case !g.played[ep.EpisodeNumber]:
			s.log.Debug().Msg(fmt.Sprintf("'%s' isn't eligible: episode %d isn't watched yet", title, ep.EpisodeNumber))
			return dueItem{}, false
		}
		target.episodeIDs = append(target.episodeIDs, ep.ID)
		if !files[ep.EpisodeFileID] {
			files[ep.EpisodeFileID] = true
			target.episodeFileIDs = append(target.episodeFileIDs, ep.EpisodeFileID)
		}
	}

	s.log.Info().Msg(fmt.Sprintf("'%s' is fully watched and past its %s grace period (last stopped playing %s)", title, s.tvGracePeriod, g.stoppedAt.Local().Format("2006-01-02 15:04")))
	d.resolved = true
	d.season = target
	return d, true
}

// seasonTarget is everything needed to delete one season through Sonarr.
type seasonTarget struct {
	seriesID       int
	seasonNumber   int
	episodeIDs     []int
	episodeFileIDs []int
}

// deleteSeason unmonitors the season first, then deletes its files — so if
// unmonitoring fails nothing is deleted, and Sonarr never sees a monitored
// season with missing files it would immediately re-download.
func (s *sweeper) deleteSeason(t *seasonTarget) error {
	if err := s.arr.unmonitorSeason(t.seriesID, t.seasonNumber, t.episodeIDs); err != nil {
		return fmt.Errorf("sonarr: %w — nothing was deleted", err)
	}
	return s.arr.deleteEpisodeFiles(t.episodeFileIDs)
}
