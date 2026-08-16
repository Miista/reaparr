package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// arrClient talks to Radarr/Sonarr only. It must never be given
// qBittorrent credentials or access — see plan.md's
// deletion-scope decision. Deleting the Radarr/Sonarr-side file only drops
// one of two hardlinks; the downloads-side copy and its seed are untouched.
type arrClient struct {
	radarrURL    string
	radarrAPIKey string
	sonarrURL    string
	sonarrAPIKey string
	httpClient   *http.Client
	log          *slog.Logger
}

// deleteMovie unmonitors and deletes a Radarr movie's file, via
// DELETE /api/v3/movie/{id}?deleteFiles=true&addImportExclusion=false.
func (a *arrClient) deleteMovie(id string) error {
	url := fmt.Sprintf("%s/api/v3/movie/%s?deleteFiles=true&addImportExclusion=false", a.radarrURL, id)
	return a.doDelete("radarr", url, a.radarrAPIKey, id)
}

// deleteSeries unmonitors and deletes a Sonarr series' files, via
// DELETE /api/v3/series/{id}?deleteFiles=true&addImportListExclusion=false.
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
