package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// jellyfinPlayedPayload is a best-guess shape based on the official Jellyfin
// Webhook plugin's documented template variables. NOT yet verified against a
// real payload — the plugin isn't installed yet as of this writing. The raw
// body is logged at debug level unconditionally so the real shape can be
// confirmed and this struct corrected before relying on it.
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
	log   *slog.Logger
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.log.Warn("rejected non-POST request", "method", r.Method, "remote_addr", r.RemoteAddr)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error("failed to read request body", "error", err)
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	h.log.Debug("received payload", "body", string(body))

	var payload jellyfinPlayedPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.log.Error("invalid JSON payload", "error", err, "body", string(body))
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if payload.NotificationType != "PlaybackStop" && payload.NotificationType != "ItemMarkedPlayed" {
		h.log.Debug("ignoring notification", "reason", "unrelated notification type", "notification_type", payload.NotificationType)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !payload.PlayedToComplete && payload.NotificationType == "PlaybackStop" {
		h.log.Info("ignoring notification", "reason", "playback not completed", "item_type", payload.ItemType, "title", payload.Name)
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
		h.log.Warn("ignoring notification", "reason", "unhandled item type", "item_type", payload.ItemType, "title", payload.Name)
		w.WriteHeader(http.StatusOK)
		return
	}
	event.User = payload.NotificationUser
	event.WatchedAt = time.Now().UTC()

	if err := h.store.recordWatched(event); err != nil {
		h.log.Error("failed to record watched event", "error", err, "kind", event.Kind, "item_id", event.ItemID, "title", event.Title)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("recorded watched event", "kind", event.Kind, "item_id", event.ItemID, "title", event.Title, "user", event.User)
	w.WriteHeader(http.StatusOK)
}
