package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// arrClient talks to Radarr/Sonarr only. It must never be given
// qBittorrent credentials or access — see watch-cleanup-tool-plan.md's
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
	a.log.Info("sending delete request", "service", service, "id", id, "url", url)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		a.log.Error("failed to build delete request", "service", service, "id", id, "error", err)
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.log.Error("delete request failed", "service", service, "id", id, "error", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		a.log.Error("delete request rejected", "service", service, "id", id, "status", resp.Status, "response", body)
		return fmt.Errorf("delete failed: %s: %v", resp.Status, body)
	}

	a.log.Debug("delete request succeeded", "service", service, "id", id, "status", resp.Status)
	return nil
}
