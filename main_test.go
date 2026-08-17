package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWaitForHardlinkPrecondition_PassesImmediatelyWhenEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
	}))
	defer srv.Close()

	arr := &arrClient{
		radarrURL: srv.URL, radarrAPIKey: "radarr-key",
		sonarrURL: srv.URL, sonarrAPIKey: "sonarr-key",
		httpClient: srv.Client(), log: testLogger(t),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok := waitForHardlinkPrecondition(ctx, arr, testLogger(t), time.Hour)
	if !ok {
		t.Fatal("expected precondition to pass immediately when hardlinks are enabled for both services")
	}
}

func TestWaitForHardlinkPrecondition_RadarrOnly_IgnoresSonarr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
	}))
	defer srv.Close()

	// Sonarr not configured at all (no API key) — its hardlink setting
	// must never be checked, since there's nothing to protect.
	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "radarr-key", sonarrAPIKey: "", httpClient: srv.Client(), log: testLogger(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok := waitForHardlinkPrecondition(ctx, arr, testLogger(t), time.Hour)
	if !ok {
		t.Fatal("expected precondition to pass when only radarr is configured and has hardlinks enabled")
	}
}

// Both configured services must independently satisfy the precondition —
// Radarr being fine doesn't excuse Sonarr being misconfigured, and vice
// versa.
func TestWaitForHardlinkPrecondition_BothMustPass(t *testing.T) {
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
	}))
	defer radarrSrv.Close()
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: false})
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{
		radarrURL: radarrSrv.URL, radarrAPIKey: "radarr-key",
		sonarrURL: sonarrSrv.URL, sonarrAPIKey: "sonarr-key",
		httpClient: radarrSrv.Client(), log: testLogger(t),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	ok := waitForHardlinkPrecondition(ctx, arr, testLogger(t), time.Hour)
	if ok {
		t.Fatal("expected precondition to fail: sonarr has hardlinks disabled even though radarr is fine")
	}
}

func TestWaitForHardlinkPrecondition_RetriesUntilFixed(t *testing.T) {
	var mu sync.Mutex
	enabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		e := enabled
		mu.Unlock()
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: e})
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "radarr-key", httpClient: srv.Client(), log: testLogger(t)}

	// Flip it to enabled shortly after the first check would have failed,
	// using a very short recheck interval so the test runs fast.
	go func() {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		enabled = true
		mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok := waitForHardlinkPrecondition(ctx, arr, testLogger(t), 10*time.Millisecond)
	if !ok {
		t.Fatal("expected precondition to eventually pass once hardlinks got enabled")
	}
}

func TestWaitForHardlinkPrecondition_ReturnsFalseOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: false})
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "radarr-key", httpClient: srv.Client(), log: testLogger(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	ok := waitForHardlinkPrecondition(ctx, arr, testLogger(t), time.Hour)
	if ok {
		t.Fatal("expected precondition wait to be cancelled, not pass")
	}
}
