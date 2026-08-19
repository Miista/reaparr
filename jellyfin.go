package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// jellyfinClient polls Jellyfin for played items and playback-stopped
// activity. Auto-discovers users via /Users rather than requiring a
// configured list — any one account's played state is sufficient (see
// sweep.go), so there is no meaningful subset of users to exclude in a
// multi-user deployment.
type jellyfinClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        zerolog.Logger
}

type jellyfinUser struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

type jellyfinItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"` // "Movie" or "Episode"
	SeriesID    string            `json:"SeriesId"`
	SeriesName  string            `json:"SeriesName"`
	ProviderIds jellyfinProviders `json:"ProviderIds"`
	UserData    jellyfinUserData  `json:"UserData"`
}

// jellyfinProviders are the external-database IDs Jellyfin tracks for an
// item. These are the join key back to Radarr/Sonarr: Jellyfin's own
// internal item ID means nothing to Radarr/Sonarr, but TMDB/TVDB IDs are
// shared across all three. A movie's own Tmdb ID is used directly; an
// episode only carries the Tvdb ID of the *episode* itself, not the
// series, so episodes require a follow-up lookup of the series item for
// its Tvdb ID (matching Sonarr, which tracks series-level, not per-episode).
type jellyfinProviders struct {
	Tmdb string `json:"Tmdb"`
	Tvdb string `json:"Tvdb"`
	Imdb string `json:"Imdb"`
}

type jellyfinUserData struct {
	Played bool `json:"Played"`
}

type jellyfinItemsResponse struct {
	Items []jellyfinItem `json:"Items"`
}

// jellyfinActivityEntry is one row from the Activity Log. Deliberately
// minimal — only what's needed to find playback-stop events and which item
// they belong to.
type jellyfinActivityEntry struct {
	Type   string    `json:"Type"`
	ItemID string    `json:"ItemId"`
	Date   time.Time `json:"Date"`
}

type jellyfinActivityResponse struct {
	Items            []jellyfinActivityEntry `json:"Items"`
	TotalRecordCount int                     `json:"TotalRecordCount"`
}

const activityLogPageSize = 200

// activityLogTypeVideoPlaybackStopped is the only event type reaparr cares
// about: it fires whenever any playback session ends, for any reason
// (finished, abandoned, disconnected) — see stoppedBeforeCutoff's doc
// comment for why that ambiguity is fine here.
const activityLogTypeVideoPlaybackStopped = "VideoPlaybackStopped"

func (c *jellyfinClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("jellyfin request to %s failed: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// users returns every account on the server.
func (c *jellyfinClient) users() ([]jellyfinUser, error) {
	var users []jellyfinUser
	if err := c.get("/Users", &users); err != nil {
		return nil, err
	}
	return users, nil
}

// playedItems returns every movie/episode a given user currently has
// marked played, including ProviderIds for matching back to Radarr/Sonarr.
// This is always live state — Jellyfin flips Played back to false if a
// title is rewatched and abandoned partway, so re-polling every sweep
// (rather than caching a past "watched" observation) is what makes it safe
// to never resurrect a stale/reversed watch state.
func (c *jellyfinClient) playedItems(userID string) ([]jellyfinItem, error) {
	path := fmt.Sprintf(
		"/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Filters=IsPlayed&Fields=SeriesId,ProviderIds",
		userID,
	)
	var resp jellyfinItemsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// seriesTvdbID looks up a series item directly to get its own Tvdb ID — an
// episode's own ProviderIds only carries the episode's Tvdb ID, not the
// series'. Sonarr tracks/deletes at the series level, so this is the ID
// that actually matters for matching.
func (c *jellyfinClient) seriesTvdbID(seriesID string) (string, error) {
	path := fmt.Sprintf("/Items/%s?Fields=ProviderIds", seriesID)
	var item jellyfinItem
	if err := c.get(path, &item); err != nil {
		return "", err
	}
	return item.ProviderIds.Tvdb, nil
}

// latestStopEvents returns, for every item with at least one
// VideoPlaybackStopped activity-log event, that event's MOST RECENT
// timestamp — unconditionally, with no cutoff/grace-period filtering here.
// An item can have multiple stop events (an abandoned attempt, then a
// later real finish) — only the latest one reflects current reality; see
// sweep_test.go's TestSweepOnce_UsesLatestStopEvent_WhenMultipleExist for
// why using an older one would cause premature deletion.
//
// Movies and TV have independently configurable grace periods (see
// config.go's MoviesGracePeriod/TVGracePeriod), and this function doesn't
// know an item's kind — that's only known once matched against Jellyfin's
// played-items list, which does carry Type. So the cutoff comparison
// happens in sweep.go, per item, using whichever grace period applies to
// its kind; this function only ever reports raw timestamps.
//
// The Activity Log is returned newest-first (confirmed against this
// server), so the first VideoPlaybackStopped entry seen for a given item
// IS its latest one — every later occurrence of that same item ID is
// guaranteed older and can be skipped outright without comparing
// timestamps. seenItem tracks which IDs have already been resolved so we
// only ever record each item once.
//
// This is deliberately not "the moment it finished watching" on its own —
// VideoPlaybackStopped fires on any stop, including someone quitting
// partway through. It only becomes meaningful once intersected against
// Jellyfin's own live Played=true state (see sweep.go): an item only
// matters here if it is CURRENTLY played, so a stale/aborted stop event
// that was never followed by a real completion is naturally excluded by
// that intersection, not by anything in this function.
//
// This server's Jellyfin version (confirmed via its actual tagged source,
// not just current upstream docs) has no server-side event-type or
// max-date filter on the Activity Log endpoint, so this fetches the whole
// log and filters by type client-side.
func (c *jellyfinClient) latestStopEvents() (map[string]time.Time, error) {
	latestStop := make(map[string]time.Time)
	seenItem := make(map[string]bool)

	startIndex := 0
	for {
		path := fmt.Sprintf("/System/ActivityLog/Entries?startIndex=%d&limit=%d", startIndex, activityLogPageSize)
		var resp jellyfinActivityResponse
		if err := c.get(path, &resp); err != nil {
			return nil, fmt.Errorf("fetch activity log at offset %d: %w", startIndex, err)
		}

		for _, e := range resp.Items {
			if e.Type != activityLogTypeVideoPlaybackStopped {
				continue
			}
			if e.ItemID == "" || seenItem[e.ItemID] {
				continue
			}
			seenItem[e.ItemID] = true
			latestStop[e.ItemID] = e.Date
		}

		startIndex += len(resp.Items)
		if len(resp.Items) == 0 || startIndex >= resp.TotalRecordCount {
			break
		}
	}

	return latestStop, nil
}
