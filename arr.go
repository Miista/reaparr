package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// arrClient talks to Radarr/Sonarr only. It must never be given
// qBittorrent credentials or access. Radarr/Sonarr import media via
// hardlink, so deleting the Radarr/Sonarr-side file only drops one of two
// hardlinks; the downloads-side copy and its seed are left untouched.
type arrClient struct {
	radarrURL    string
	radarrAPIKey string
	sonarrURL    string
	sonarrAPIKey string
	httpClient   *http.Client
	log          *slog.Logger
}

// radarrMovie/sonarrSeries are the small subset of fields needed to match a
// Jellyfin item back to Radarr/Sonarr's own internal numeric ID — Jellyfin's
// item ID is meaningless to Radarr/Sonarr, but tmdbId/tvdbId are shared
// across all three systems.
type radarrMovie struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	TmdbID int    `json:"tmdbId"`
}

type sonarrSeries struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	TvdbID int    `json:"tvdbId"`
}

// findMovieByTmdbID looks up Radarr's internal movie ID for a given TMDB
// ID. Returns ok=false if Radarr doesn't have this movie at all — that's a
// real, expected case (Jellyfin might see something Radarr doesn't track),
// not treated as an error.
func (a *arrClient) findMovieByTmdbID(tmdbID string) (movie radarrMovie, ok bool, err error) {
	var movies []radarrMovie
	if err := a.get(a.radarrURL+"/api/v3/movie", a.radarrAPIKey, &movies); err != nil {
		return radarrMovie{}, false, err
	}
	for _, m := range movies {
		if fmt.Sprint(m.TmdbID) == tmdbID {
			return m, true, nil
		}
	}
	return radarrMovie{}, false, nil
}

// findSeriesByTvdbID looks up Sonarr's internal series ID for a given TVDB
// ID. Returns ok=false if Sonarr doesn't track this series.
func (a *arrClient) findSeriesByTvdbID(tvdbID string) (series sonarrSeries, ok bool, err error) {
	var allSeries []sonarrSeries
	if err := a.get(a.sonarrURL+"/api/v3/series", a.sonarrAPIKey, &allSeries); err != nil {
		return sonarrSeries{}, false, err
	}
	for _, s := range allSeries {
		if fmt.Sprint(s.TvdbID) == tvdbID {
			return s, true, nil
		}
	}
	return sonarrSeries{}, false, nil
}

func (a *arrClient) get(url, apiKey string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("request to %s failed: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// deleteMovie unmonitors and deletes a Radarr movie's file, via
// DELETE /api/v3/movie/{id}?deleteFiles=true&addImportExclusion=false. id
// must be Radarr's own internal numeric ID, not Jellyfin's item ID or a
// TMDB ID — see findMovieByTmdbID.
func (a *arrClient) deleteMovie(id string) error {
	url := fmt.Sprintf("%s/api/v3/movie/%s?deleteFiles=true&addImportExclusion=false", a.radarrURL, id)
	return a.doDelete("radarr", url, a.radarrAPIKey, id)
}

// deleteSeries unmonitors and deletes a Sonarr series' files, via
// DELETE /api/v3/series/{id}?deleteFiles=true&addImportListExclusion=false.
// id must be Sonarr's own internal numeric ID — see findSeriesByTvdbID.
func (a *arrClient) deleteSeries(id string) error {
	url := fmt.Sprintf("%s/api/v3/series/%s?deleteFiles=true&addImportListExclusion=false", a.sonarrURL, id)
	return a.doDelete("sonarr", url, a.sonarrAPIKey, id)
}

func (a *arrClient) doDelete(service, url, apiKey, id string) error {
	a.log.Info(fmt.Sprintf("calling %s to delete this item and its file — this is the actual deletion happening now", service), "service", service, "item_id", id, "request_url", url)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		a.log.Error("could not even build the delete request — this is a bug, nothing was sent", "service", service, "item_id", id, "error", err)
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.log.Error(fmt.Sprintf("could not reach %s to send the delete request — nothing was deleted", service), "service", service, "item_id", id, "error", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		a.log.Error(fmt.Sprintf("%s refused the delete request — nothing was deleted", service), "service", service, "item_id", id, "http_status", resp.Status, "response_body", body)
		return fmt.Errorf("delete failed: %s: %v", resp.Status, body)
	}

	a.log.Info(fmt.Sprintf("%s confirmed the delete — file and hardlink are gone from its library, qBittorrent/downloads copy is untouched", service), "service", service, "item_id", id, "http_status", resp.Status)
	return nil
}
