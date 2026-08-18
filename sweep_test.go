package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJellyfinConfig describes everything a test's fake Jellyfin server
// needs to serve: which users exist, what each has played, and what
// playback-stopped events are in the activity log.
type fakeJellyfinConfig struct {
	users           []jellyfinUser
	itemsByUser     map[string][]jellyfinItem // userID -> played items
	activityEntries []jellyfinActivityEntry
	seriesByID      map[string]jellyfinItem // seriesID -> series item (for seriesTvdbID lookups)
}

func newFakeJellyfin(t *testing.T, cfg fakeJellyfinConfig) *jellyfinClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			json.NewEncoder(w).Encode(cfg.users)

		case strings.HasSuffix(r.URL.Path, "/Items") && strings.HasPrefix(r.URL.Path, "/Users/"):
			userID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/Users/"), "/Items")
			json.NewEncoder(w).Encode(jellyfinItemsResponse{Items: cfg.itemsByUser[userID]})

		case strings.HasPrefix(r.URL.Path, "/Items/"):
			seriesID := strings.TrimPrefix(r.URL.Path, "/Items/")
			item, ok := cfg.seriesByID[seriesID]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(item)

		case r.URL.Path == "/System/ActivityLog/Entries":
			// Real Jellyfin always returns these newest-first, hardcoded
			// server-side with no way to override it (confirmed against
			// this exact server version's source) — latestStopEvents
			// relies on that ordering, so the fake must enforce it too,
			// regardless of what order a test lists activityEntries in.
			sorted := append([]jellyfinActivityEntry(nil), cfg.activityEntries...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date.After(sorted[j].Date) })

			startIndex, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			end := startIndex + limit
			if end > len(sorted) {
				end = len(sorted)
			}
			var page []jellyfinActivityEntry
			if startIndex < len(sorted) {
				page = sorted[startIndex:end]
			}
			json.NewEncoder(w).Encode(jellyfinActivityResponse{Items: page, TotalRecordCount: len(cfg.activityEntries)})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &jellyfinClient{baseURL: srv.URL, apiKey: "k", httpClient: srv.Client(), log: testLogger(t)}
}

// newFakeArrCatalog serves a fixed Radarr movie / Sonarr series catalog for
// GET /api/v3/movie and /api/v3/series (used by findMovieByTmdbID /
// findSeriesByTvdbID), and accepts any DELETE without complaint.
// newFakeArrCatalog serves a fixed Radarr/Sonarr catalog and reports
// hardlinks as enabled by default (safe to delete) — tests that
// specifically exercise the unsafe path build their own fake instead.
func newFakeArrCatalog(t *testing.T, movies []radarrMovie, series []sonarrSeries) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			json.NewEncoder(w).Encode(movies)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/series":
			json.NewEncoder(w).Encode(series)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/mediamanagement":
			json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withHardlinksEnabled wraps a handler so that GET
// /api/v3/config/mediamanagement reports hardlinks as enabled, falling
// through to next for everything else. Every fake Radarr/Sonarr server in
// this file needs to answer this endpoint now that checkHardlinkSafety
// runs at the start of every sweep — this keeps that boilerplate in one
// place rather than repeating it in every inline fake.
func withHardlinksEnabled(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/mediamanagement" {
			json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: true})
			return
		}
		next(w, r)
	}
}

func newTestArr(t *testing.T, radarrSrv, sonarrSrv *httptest.Server) *arrClient {
	t.Helper()
	return &arrClient{
		radarrURL: radarrSrv.URL, radarrAPIKey: "k",
		sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k",
		httpClient: radarrSrv.Client(), log: testLogger(t),
	}
}

func TestSweepOnce_DeletesMovieWatchedAndStoppedBeforeGracePeriod(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{
				ID: "jf-movie-1", Name: "Old Movie", Type: "Movie",
				ProviderIds: jellyfinProviders{Tmdb: "999"},
				UserData:    jellyfinUserData{Played: true},
			}},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)},
		},
	})

	var deleteCalls int32
	deleteTrackingSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Old Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer deleteTrackingSrv.Close()

	arr := &arrClient{radarrURL: deleteTrackingSrv.URL, radarrAPIKey: "k", sonarrURL: deleteTrackingSrv.URL, sonarrAPIKey: "k", httpClient: deleteTrackingSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected 1 delete call, got %d", got)
	}
}

func TestSweepOnce_LeavesItemAlone_StoppedButNotPastGracePeriod(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Recent Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: []jellyfinActivityEntry{
			// stopped only 1 hour ago — well within a 7-day grace period
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-1 * time.Hour)},
		},
	})

	var deleteCalls int32
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Recent Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "k", sonarrURL: arrSrv.URL, sonarrAPIKey: "k", httpClient: arrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 7 * 24 * time.Hour, tvGracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 delete calls, item hasn't cleared its grace period yet, got %d", got)
	}
}

// This is the core safety property of the whole design: an item that
// stopped playing long ago but ISN'T currently Played=true (abandoned
// partway, or unplayed again after a partial rewatch) must never be
// deleted — set A alone (stop events) says nothing about completion.
func TestSweepOnce_IgnoresOldStopEvent_IfNotCurrentlyPlayed(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {}, // nothing currently played
		},
		activityEntries: []jellyfinActivityEntry{
			// stopped (abandoned) long ago, but never finished
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)},
		},
	})

	var deleteCalls int32
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "k", sonarrURL: arrSrv.URL, sonarrAPIKey: "k", httpClient: arrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 delete/lookup calls for an item that isn't currently played, got %d", got)
	}
}

// Mirrors the real bug hit in production: an abandoned session (stop event,
// Played=false) followed by a genuine finish later must be judged on
// CURRENT Played state, not any specific historical stop event alone.
func TestSweepOnce_UsesLatestStopEvent_WhenMultipleExist(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Rewatched Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-72 * time.Hour)}, // abandoned attempt
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-2 * time.Hour)},  // the real finish
		},
	})

	var deleteCalls int32
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Rewatched Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "k", sonarrURL: arrSrv.URL, sonarrAPIKey: "k", httpClient: arrSrv.Client(), log: testLogger(t)}
	// grace period of 24h: the latest (real) stop event is only 2h old, so
	// this must NOT be deleted yet, even though the OLDER stop event is
	// well past 24h. Using the wrong (earliest) stop event would wrongly
	// delete this.
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 delete calls — latest stop event hasn't cleared the grace period, got %d", got)
	}
}

func TestSweepOnce_RoutesEpisodeToSonarr_ViaSeriesTvdbID(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{
				ID: "jf-episode-1", Name: "S01E01", Type: "Episode",
				SeriesID: "jf-series-1", SeriesName: "Some Show",
				ProviderIds: jellyfinProviders{Tvdb: "episode-level-id-not-used"},
				UserData:    jellyfinUserData{Played: true},
			}},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-episode-1", Date: now.Add(-48 * time.Hour)},
		},
		seriesByID: map[string]jellyfinItem{
			"jf-series-1": {ID: "jf-series-1", Type: "Series", ProviderIds: jellyfinProviders{Tvdb: "410092"}},
		},
	})

	var gotPath string
	radarrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		// Only the hardlink-safety check (a GET to config/mediamanagement,
		// handled by withHardlinksEnabled) should ever reach radarr here —
		// anything else means radarr was wrongly consulted for an episode.
		t.Fatalf("radarr should not be called for anything beyond the hardlink check when handling an episode, got %s %s", r.Method, r.URL.Path)
	}))
	defer radarrSrv.Close()
	sonarrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/series" {
			json.NewEncoder(w).Encode([]sonarrSeries{{ID: 7, Title: "Some Show", TvdbID: 410092}})
			return
		}
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: radarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: sonarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if gotPath != "/api/v3/series/7" {
		t.Fatalf("sonarr path = %q, want /api/v3/series/7", gotPath)
	}
}

func TestSweepOnce_MovieNotInRadarr_IsSkippedNotRetried(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Untracked Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "12345"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)},
		},
	})

	arrSrv := newFakeArrCatalog(t, []radarrMovie{{ID: 1, Title: "Some Other Movie", TmdbID: 999}}, nil)
	arr := newTestArr(t, arrSrv, arrSrv)
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	// Should not panic or error out — just log a warning and move on.
	sw.sweepOnce()
}

// A Radarr-only or Sonarr-only deployment (movies-only or TV-only
// household) is a legitimate configuration — a watched episode must be
// cleanly ignored (no crash, no Jellyfin series lookup, no Sonarr API
// calls) when Sonarr isn't configured at all, not treated as an error.
func TestSweepOnce_EpisodeIgnored_WhenSonarrNotConfigured(t *testing.T) {
	now := time.Now().UTC()

	seriesLookupCalled := false
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			json.NewEncoder(w).Encode([]jellyfinUser{{ID: "u1", Name: "admin"}})
		case r.URL.Path == "/Users/u1/Items":
			json.NewEncoder(w).Encode(jellyfinItemsResponse{Items: []jellyfinItem{
				{
					ID: "jf-episode-1", Name: "S01E01", Type: "Episode",
					SeriesID: "jf-series-1", SeriesName: "Some Show",
					UserData: jellyfinUserData{Played: true},
				},
			}})
		case r.URL.Path == "/System/ActivityLog/Entries":
			json.NewEncoder(w).Encode(jellyfinActivityResponse{
				Items: []jellyfinActivityEntry{
					{Type: "VideoPlaybackStopped", ItemID: "jf-episode-1", Date: now.Add(-48 * time.Hour)},
				},
				TotalRecordCount: 1,
			})
		case r.URL.Path == "/Items/jf-series-1":
			seriesLookupCalled = true
			json.NewEncoder(w).Encode(jellyfinItem{ID: "jf-series-1", Type: "Series", ProviderIds: jellyfinProviders{Tvdb: "410092"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jfSrv.Close()
	jellyfin := &jellyfinClient{baseURL: jfSrv.URL, apiKey: "k", httpClient: jfSrv.Client(), log: testLogger(t)}

	sonarrCalled := false
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		// Only the hardlink-safety check (handled by withHardlinksEnabled,
		// for radarr since it's configured here) should ever land here —
		// anything else means sonarr was wrongly consulted.
		sonarrCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "radarr-key", sonarrURL: arrSrv.URL, sonarrAPIKey: "", httpClient: arrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if seriesLookupCalled {
		t.Fatal("expected no series lookup when sonarr isn't configured")
	}
	if sonarrCalled {
		t.Fatal("expected no sonarr API calls when sonarr isn't configured")
	}
}

func TestSweepOnce_ActivityLogPagination(t *testing.T) {
	now := time.Now().UTC()

	// Build more entries than one page to exercise pagination.
	var entries []jellyfinActivityEntry
	for i := 0; i < activityLogPageSize+10; i++ {
		entries = append(entries, jellyfinActivityEntry{Type: "SessionStarted", ItemID: "", Date: now.Add(-time.Duration(i) * time.Minute)})
	}
	entries = append(entries, jellyfinActivityEntry{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)})

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Paginated Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: entries,
	})

	var deleteCalls int32
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Paginated Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := &arrClient{radarrURL: arrSrv.URL, radarrAPIKey: "k", sonarrURL: arrSrv.URL, sonarrAPIKey: "k", httpClient: arrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected the paginated-in stop event to be found and deleted, got %d delete calls", got)
	}
}

// A movies-only misconfiguration (Radarr not using hardlinks) must not
// also block TV cleanup that's actually safe — each service is gated
// independently, not as an all-or-nothing precondition.
func TestSweepOnce_RadarrUnsafe_DoesNotBlockSonarr(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {
				{ID: "jf-movie-1", Name: "Old Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}},
				{
					ID: "jf-episode-1", Name: "S01E01", Type: "Episode",
					SeriesID: "jf-series-1", SeriesName: "Some Show",
					UserData: jellyfinUserData{Played: true},
				},
			},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)},
			{Type: "VideoPlaybackStopped", ItemID: "jf-episode-1", Date: now.Add(-48 * time.Hour)},
		},
		seriesByID: map[string]jellyfinItem{
			"jf-series-1": {ID: "jf-series-1", Type: "Series", ProviderIds: jellyfinProviders{Tvdb: "410092"}},
		},
	})

	var movieDeletes, seriesDeletes int32
	radarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/mediamanagement" {
			json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: false}) // radarr: unsafe
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&movieDeletes, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer radarrSrv.Close()

	sonarrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) { // sonarr: safe
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/series" {
			json.NewEncoder(w).Encode([]sonarrSeries{{ID: 7, Title: "Some Show", TvdbID: 410092}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&seriesDeletes, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: radarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: radarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&movieDeletes); got != 0 {
		t.Fatalf("expected 0 movie deletes since radarr is unsafe, got %d", got)
	}
	if got := atomic.LoadInt32(&seriesDeletes); got != 1 {
		t.Fatalf("expected 1 series delete since sonarr is safe, got %d", got)
	}
}

// Same as above, mirrored: Sonarr unsafe must not block Radarr.
func TestSweepOnce_SonarrUnsafe_DoesNotBlockRadarr(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Old Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-48 * time.Hour)},
		},
	})

	var movieDeletes int32
	radarrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) { // radarr: safe
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Old Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&movieDeletes, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer radarrSrv.Close()

	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/mediamanagement" {
			json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: false}) // sonarr: unsafe
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: radarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: radarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&movieDeletes); got != 1 {
		t.Fatalf("expected 1 movie delete since radarr is safe, got %d", got)
	}
}

// checkHardlinkSafety is called once per sweep, not once per item — an
// API error checking the setting should conservatively treat that service
// as unsafe for this sweep, not crash or silently assume safe.
func TestCheckHardlinkSafety_TreatsCheckFailureAsUnsafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	sw := &sweeper{arr: arr, log: testLogger(t)}

	safety := sw.checkHardlinkSafety()
	if safety.radarrSafe {
		t.Fatal("expected radarrSafe=false when the hardlink check itself fails")
	}
}

// A movie past its (short) movies grace period, but not yet past a
// longer TV grace period, must still be deleted — the two settings are
// genuinely independent, not "whichever is longer wins" or similar.
func TestSweepOnce_UsesShorterMoviesGracePeriod(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{ID: "jf-movie-1", Name: "Quick Movie", Type: "Movie", ProviderIds: jellyfinProviders{Tmdb: "999"}, UserData: jellyfinUserData{Played: true}}},
		},
		activityEntries: []jellyfinActivityEntry{
			// stopped 3 days ago: past a 2-day movies grace period, but
			// NOT past a 7-day TV grace period.
			{Type: "VideoPlaybackStopped", ItemID: "jf-movie-1", Date: now.Add(-3 * 24 * time.Hour)},
		},
	})

	var deleteCalls int32
	arrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie" {
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Quick Movie", TmdbID: 999}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer arrSrv.Close()

	arr := newTestArr(t, arrSrv, arrSrv)
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 2 * 24 * time.Hour, tvGracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 1 {
		t.Fatalf("expected 1 delete: movie is past the 2-day movies grace period, got %d", got)
	}
}

// Mirrors the above: an episode past a shorter movies grace period but
// not yet past the longer TV grace period must NOT be deleted — episodes
// are always judged against tvGracePeriod, never moviesGracePeriod.
func TestSweepOnce_UsesLongerTVGracePeriod(t *testing.T) {
	now := time.Now().UTC()

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{
				ID: "jf-episode-1", Name: "S01E01", Type: "Episode",
				SeriesID: "jf-series-1", SeriesName: "Some Show",
				UserData: jellyfinUserData{Played: true},
			}},
		},
		activityEntries: []jellyfinActivityEntry{
			// stopped 3 days ago: past a 2-day movies grace period, but
			// NOT past a 7-day TV grace period. If episodes were wrongly
			// judged against moviesGracePeriod, this would be deleted.
			{Type: "VideoPlaybackStopped", ItemID: "jf-episode-1", Date: now.Add(-3 * 24 * time.Hour)},
		},
		seriesByID: map[string]jellyfinItem{
			"jf-series-1": {ID: "jf-series-1", Type: "Series", ProviderIds: jellyfinProviders{Tvdb: "410092"}},
		},
	})

	var deleteCalls int32
	sonarrSrv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/series" {
			json.NewEncoder(w).Encode([]sonarrSeries{{ID: 7, Title: "Some Show", TvdbID: 410092}})
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalls, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sonarrSrv.Close()

	arr := &arrClient{radarrURL: sonarrSrv.URL, radarrAPIKey: "k", sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: sonarrSrv.Client(), log: testLogger(t)}
	sw := &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 2 * 24 * time.Hour, tvGracePeriod: 7 * 24 * time.Hour, log: testLogger(t)}

	sw.sweepOnce()

	if got := atomic.LoadInt32(&deleteCalls); got != 0 {
		t.Fatalf("expected 0 deletes: episode is within the 7-day TV grace period even though it's past the 2-day movies grace period, got %d", got)
	}
}

func TestGracePeriodFor_MovieUsesMoviesGracePeriod(t *testing.T) {
	sw := &sweeper{moviesGracePeriod: 2 * 24 * time.Hour, tvGracePeriod: 7 * 24 * time.Hour}
	got := sw.gracePeriodFor(jellyfinItem{Type: "Movie"})
	if got != 2*24*time.Hour {
		t.Fatalf("expected movies grace period, got %v", got)
	}
}

func TestGracePeriodFor_EpisodeUsesTVGracePeriod(t *testing.T) {
	sw := &sweeper{moviesGracePeriod: 2 * 24 * time.Hour, tvGracePeriod: 7 * 24 * time.Hour}
	got := sw.gracePeriodFor(jellyfinItem{Type: "Episode"})
	if got != 7*24*time.Hour {
		t.Fatalf("expected TV grace period, got %v", got)
	}
}
