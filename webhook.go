package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

// jellyfinPlayedPayload is a best-guess shape based on the official Jellyfin
// Webhook plugin's documented template variables. NOT yet verified against a
// real payload — the plugin isn't installed yet as of this writing. Every
// request is logged in full below so the real shape can be confirmed and
// this struct corrected before relying on it.
type jellyfinPlayedPayload struct {
	NotificationType string `json:"NotificationType"`
	ItemType         string `json:"ItemType"` // "Movie" or "Episode"
	Name             string `json:"Name"`
	SeriesName       string `json:"SeriesName"`
	ItemID           string `json:"ItemId"`
	SeriesID         string `json:"SeriesId"`
	NotificationUser string `json:"NotificationUsername"`
	PlayedToComplete bool   `json:"PlayedToCompletion"`
}

type webhookHandler struct {
	store *store
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// Logged unconditionally until the real payload shape is confirmed
	// against an installed Webhook plugin (see watch-cleanup-tool-plan.md).
	log.Printf("webhook payload: %s", body)

	var payload jellyfinPlayedPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if payload.NotificationType != "PlaybackStop" && payload.NotificationType != "ItemMarkedPlayed" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !payload.PlayedToComplete && payload.NotificationType == "PlaybackStop" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var event watchedEvent
	switch payload.ItemType {
	case "Movie":
		event = watchedEvent{
			Kind:   kindMovie,
			ItemID: payload.ItemID,
			Title:  payload.Name,
		}
	case "Episode":
		event = watchedEvent{
			Kind:   kindSeries,
			ItemID: payload.SeriesID,
			Title:  payload.SeriesName,
		}
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	event.User = payload.NotificationUser
	event.WatchedAt = time.Now().UTC()

	if err := h.store.recordWatched(event); err != nil {
		log.Printf("record watched event: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
