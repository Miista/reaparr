package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/rs/zerolog"
)

// seerrClient talks to Seerr only, and only to clean up requests left
// orphaned by Seerr's own "Media Availability Sync" job — see
// SEERR_PLAN.md for why this gap exists and why Reaparr closes it.
type seerrClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        zerolog.Logger
}

// hasSeerr reports whether Seerr cleanup was configured at all — entirely
// optional, same pattern as arrClient.hasRadarr/hasSonarr.
func (c *seerrClient) hasSeerr() bool { return c.apiKey != "" }

type seerrMedia struct {
	ID        int    `json:"id"`
	TmdbID    int    `json:"tmdbId"`
	TvdbID    int    `json:"tvdbId"`
	Status    int    `json:"status"`
	MediaType string `json:"mediaType"` // "movie" or "tv"
}

type seerrRequest struct {
	ID    int        `json:"id"`
	Media seerrMedia `json:"media"`
}

type seerrPageInfo struct {
	Pages   int `json:"pages"`
	Results int `json:"results"`
}

type seerrRequestsResponse struct {
	PageInfo seerrPageInfo  `json:"pageInfo"`
	Results  []seerrRequest `json:"results"`
}

const seerrRequestPageSize = 50

// seerrStaleMedia is one Seerr media record whose title has been deleted
// (media.status == DELETED) but whose request record is still sitting
// around due to the Availability Sync gap — ready for cleanup.
type seerrStaleMedia struct {
	mediaID int
	title   string
}

// deletedRequests paginates GET /request?filter=deleted — confirmed
// server-side filtered (see SEERR_PLAN.md) — and returns one entry per
// distinct stale Media record, deduplicated (multiple requests can share
// the same underlying media; see SEERR_PLAN.md's "multiple requests"
// discussion).
//
// The response's nested media object carries no title field, only
// tmdbId/tvdbId/status — so a title for the log line is fetched with one
// supplementary lookup per distinct media record via titleFor.
func (c *seerrClient) deletedRequests() ([]seerrStaleMedia, error) {
	seenMedia := make(map[int]bool)
	var stale []seerrStaleMedia

	skip := 0
	for {
		path := fmt.Sprintf("/api/v1/request?take=%d&skip=%d&filter=deleted", seerrRequestPageSize, skip)
		var resp seerrRequestsResponse
		if err := c.get(path, &resp); err != nil {
			return nil, fmt.Errorf("fetch deleted requests at offset %d: %w", skip, err)
		}

		for _, r := range resp.Results {
			if seenMedia[r.Media.ID] {
				continue
			}
			seenMedia[r.Media.ID] = true

			title, err := c.titleFor(r.Media)
			if err != nil {
				c.log.Warn().Msg(fmt.Sprintf("could not look up a title for seerr media id %d, will log it by id instead: %v", r.Media.ID, err))
				title = fmt.Sprintf("media id %d", r.Media.ID)
			}
			stale = append(stale, seerrStaleMedia{mediaID: r.Media.ID, title: title})
		}

		skip += len(resp.Results)
		if len(resp.Results) == 0 || skip >= resp.PageInfo.Results {
			break
		}
	}

	return stale, nil
}

// seerrTitledMedia is the subset of Seerr's /movie/{id} and /tv/{id}
// response shapes needed for a human-readable log line — Seerr normalizes
// movie titles under "title" and TV titles under "name".
type seerrTitledMedia struct {
	Title string `json:"title"`
	Name  string `json:"name"`
}

// titleFor fetches a display title for a stale media record, purely for
// logging — the /request?filter=deleted response itself has no title
// field (see deletedRequests's doc comment), only IDs.
func (c *seerrClient) titleFor(m seerrMedia) (string, error) {
	var path string
	switch m.MediaType {
	case "movie":
		path = fmt.Sprintf("/api/v1/movie/%d", m.TmdbID)
	case "tv":
		path = fmt.Sprintf("/api/v1/tv/%d", m.TmdbID)
	default:
		return "", fmt.Errorf("unknown seerr media type %q", m.MediaType)
	}

	var titled seerrTitledMedia
	if err := c.get(path, &titled); err != nil {
		return "", err
	}
	if titled.Title != "" {
		return titled.Title, nil
	}
	return titled.Name, nil
}

func (c *seerrClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("seerr request to %s failed: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// deleteMedia deletes a Seerr media record via DELETE /api/v1/media/{id}.
// This cascades (Seerr's own ORM, cascade: ['remove']) to delete every
// associated request row too — see SEERR_PLAN.md for why deleting the
// whole media record, not individual requests, is correct here.
func (c *seerrClient) deleteMedia(mediaID int) error {
	url := fmt.Sprintf("%s/api/v1/media/%d", c.baseURL, mediaID)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		c.log.Error().Msg(fmt.Sprintf("could not build the delete request for seerr media %d — this is a bug, nothing was sent: %v", mediaID, err))
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Error().Msg(fmt.Sprintf("could not reach seerr to delete media %d — nothing was deleted: %v", mediaID, err))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		c.log.Error().Msg(fmt.Sprintf("seerr refused to delete media %d (%s) — nothing was deleted: %v", mediaID, resp.Status, body))
		return fmt.Errorf("delete failed: %s: %v", resp.Status, body)
	}

	return nil
}
