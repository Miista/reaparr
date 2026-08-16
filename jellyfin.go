package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// jellyfinClient polls Jellyfin for played items. Auto-discovers users via
// /Users rather than requiring a configured list — see the "either account"
// trigger condition in plan.md: any account's played
// state is sufficient, so there is no meaningful subset of users to
// exclude in a household deployment.
type jellyfinClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	log        *slog.Logger
}

type jellyfinUser struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

type jellyfinItem struct {
	ID         string           `json:"Id"`
	Name       string           `json:"Name"`
	Type       string           `json:"Type"` // "Movie" or "Episode"
	SeriesID   string           `json:"SeriesId"`
	SeriesName string           `json:"SeriesName"`
	UserData   jellyfinUserData `json:"UserData"`
}

type jellyfinUserData struct {
	Played         bool   `json:"Played"`
	LastPlayedDate string `json:"LastPlayedDate"`
}

type jellyfinItemsResponse struct {
	Items []jellyfinItem `json:"Items"`
}

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

// playedItems returns every movie/episode a given user has marked played,
// including UserData so LastPlayedDate is available as the grace-period
// timestamp source.
func (c *jellyfinClient) playedItems(userID string) ([]jellyfinItem, error) {
	path := fmt.Sprintf(
		"/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Filters=IsPlayed&Fields=SeriesId",
		userID,
	)
	var resp jellyfinItemsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// parseLastPlayedDate parses Jellyfin's ISO-8601 timestamp. Falls back to
// "now" if missing/unparseable — an item reported as Played but with no
// timestamp still needs a grace-period start, and "now" is the safest
// (latest, most conservative) default rather than skipping it entirely.
func parseLastPlayedDate(raw string, log *slog.Logger) time.Time {
	if raw == "" {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Warn("failed to parse LastPlayedDate, defaulting to now", "raw", raw, "error", err)
		return time.Now().UTC()
	}
	return t.UTC()
}
