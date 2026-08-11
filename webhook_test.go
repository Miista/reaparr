package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func farFuture() time.Time {
	return time.Now().UTC().Add(24 * time.Hour)
}

func postWebhook(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/jellyfin", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_ItemMarkedPlayed_Movie_IsRecorded(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{
		"NotificationType": "ItemMarkedPlayed",
		"ItemType": "Movie",
		"Name": "Test Movie",
		"ItemId": "abc123",
		"NotificationUsername": "soren"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 recorded event, got %d", len(due))
	}
	if due[0].Kind != kindMovie || due[0].ItemID != "abc123" || due[0].User != "soren" {
		t.Fatalf("unexpected event: %+v", due[0])
	}
}

func TestWebhook_ItemMarkedPlayed_Episode_UsesSeriesID(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{
		"NotificationType": "ItemMarkedPlayed",
		"ItemType": "Episode",
		"Name": "S01E01",
		"SeriesName": "Test Show",
		"ItemId": "episode-id",
		"SeriesId": "series-id",
		"NotificationUsername": "soren"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 recorded event, got %d", len(due))
	}
	if due[0].Kind != kindSeries || due[0].ItemID != "series-id" || due[0].Title != "Test Show" {
		t.Fatalf("unexpected event: %+v", due[0])
	}
}

func TestWebhook_PlaybackStop_IncompletePlay_IsIgnored(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{
		"NotificationType": "PlaybackStop",
		"ItemType": "Movie",
		"Name": "Abandoned Movie",
		"ItemId": "abc123",
		"PlayedToCompletion": false
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected incomplete playback to be ignored, got %d events", len(due))
	}
}

func TestWebhook_PlaybackStop_CompletedPlay_IsRecorded(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{
		"NotificationType": "PlaybackStop",
		"ItemType": "Movie",
		"Name": "Finished Movie",
		"ItemId": "xyz",
		"PlayedToCompletion": true
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected completed playback to be recorded, got %d events", len(due))
	}
}

func TestWebhook_UnrelatedNotificationType_IsIgnored(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{"NotificationType": "UserLoggedIn"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected unrelated notification to be ignored, got %d events", len(due))
	}
}

func TestWebhook_UnknownItemType_IsIgnored(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `{
		"NotificationType": "ItemMarkedPlayed",
		"ItemType": "Audio",
		"Name": "Some Song",
		"ItemId": "song-id"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	due, err := s.duePending(farFuture())
	if err != nil {
		t.Fatalf("duePending: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("expected unknown item type to be ignored, got %d events", len(due))
	}
}

func TestWebhook_InvalidJSON_Returns400(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	rec := postWebhook(t, h, `not json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhook_GetMethod_Returns405(t *testing.T) {
	s := newTestStore(t)
	h := &webhookHandler{store: s}

	req := httptest.NewRequest(http.MethodGet, "/webhook/jellyfin", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
