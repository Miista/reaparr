package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUnkept answers UnkeptTimes like the store would.
type fakeUnkept struct {
	times map[string]time.Time
	err   error
}

func (f fakeUnkept) UnkeptTimes() (map[string]time.Time, error) { return f.times, f.err }

// newUnkeepMovieSweeper is a sweeper over one played movie (Radarr ID 42)
// that stopped playing watchedAgo ago, with a 24h grace period. The returned
// counter is how many times Radarr was asked to delete something.
func newUnkeepMovieSweeper(t *testing.T, watchedAgo time.Duration, unkept unkeptSource) (*sweeper, *int32) {
	t.Helper()
	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "u1", Name: "admin"}},
		itemsByUser: map[string][]jellyfinItem{
			"u1": {{
				ID: "jf-movie-1", Name: "Old Movie", Type: "Movie",
				ProviderIds: jellyfinProviders{Tmdb: "999"},
				UserData:    jellyfinUserData{Played: true},
			}},
		},
		activityEntries: []jellyfinActivityEntry{stopped("jf-movie-1", watchedAgo)},
	})

	deletes := new(int32)
	srv := httptest.NewServer(withHardlinksEnabled(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
			json.NewEncoder(w).Encode([]radarrMovie{{ID: 42, Title: "Old Movie", TmdbID: 999}})
		case r.Method == http.MethodDelete:
			atomic.AddInt32(deletes, 1)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	arr := &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}
	return &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, unkept: unkept, log: testLogger(t)}, deletes
}

func TestUnkeep_GracePeriodCountsFromTheLaterOfWatchAndUnkeep(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		watchedAgo time.Duration
		unkept     map[string]time.Time
		wantDue    bool
		wantFrom   string // "watch" or "unkeep": what the grace period counts from
	}{
		{"never unkept: counts from the watch", 48 * time.Hour, nil, true, "watch"},
		{"unkept an hour ago: fresh grace period", 48 * time.Hour, map[string]time.Time{"radarr:42": now.Add(-time.Hour)}, false, "unkeep"},
		{"unkept a year after the watch, a while ago: due", 400 * 24 * time.Hour, map[string]time.Time{"radarr:42": now.Add(-30 * time.Hour)}, true, "unkeep"},
		{"watched again after the unkeep: counts from that watch", time.Hour, map[string]time.Time{"radarr:42": now.Add(-48 * time.Hour)}, false, "watch"},
		{"unkept before an old watch: counts from the watch", 48 * time.Hour, map[string]time.Time{"radarr:42": now.Add(-72 * time.Hour)}, true, "watch"},
		{"another title unkept: no effect", 48 * time.Hour, map[string]time.Time{"radarr:43": now.Add(-time.Hour), "sonarr:42": now.Add(-time.Hour)}, true, "watch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sw, _ := newUnkeepMovieSweeper(t, c.watchedAgo, fakeUnkept{times: c.unkept})
			candidates, err := sw.findCandidates()
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 {
				t.Fatalf("candidates = %+v, want the one movie", candidates)
			}
			d := candidates[0]
			if d.due != c.wantDue {
				t.Errorf("due = %v, want %v", d.due, c.wantDue)
			}
			want := d.stoppedAt
			if c.wantFrom == "unkeep" {
				want = c.unkept["radarr:42"]
			}
			if !d.graceFrom.Equal(want) {
				t.Errorf("grace period counts from %v, want the %s at %v", d.graceFrom, c.wantFrom, want)
			}
			// The dashboard's countdown must agree with the sweep.
			if got := renderDue(candidates)[0].DueAt; got != want.Add(24*time.Hour).Format(time.RFC3339) {
				t.Errorf("due_at = %s, want %s", got, want.Add(24*time.Hour).Format(time.RFC3339))
			}
		})
	}
}

func TestUnkeep_RecentlyUnkeptMovieIsNotDeleted(t *testing.T) {
	sw, deletes := newUnkeepMovieSweeper(t, 400*24*time.Hour,
		fakeUnkept{times: map[string]time.Time{"radarr:42": time.Now().Add(-time.Minute)}})
	sw.sweepOnce()
	if got := atomic.LoadInt32(deletes); got != 0 {
		t.Fatalf("deleted a movie unkept a minute ago (%d delete calls)", got)
	}
}

func TestUnkeep_MoviePastItsFreshGracePeriodIsDeleted(t *testing.T) {
	sw, deletes := newUnkeepMovieSweeper(t, 400*24*time.Hour,
		fakeUnkept{times: map[string]time.Time{"radarr:42": time.Now().Add(-25 * time.Hour)}})
	sw.sweepOnce()
	if got := atomic.LoadInt32(deletes); got != 1 {
		t.Fatalf("expected 1 delete call once the fresh grace period passed, got %d", got)
	}
}

// Not knowing when titles were unkept must stop the sweep: guessing "never"
// would delete a title unkept a moment ago.
func TestUnkeep_UnreadableTimesDeleteNothing(t *testing.T) {
	sw, deletes := newUnkeepMovieSweeper(t, 48*time.Hour, fakeUnkept{err: errors.New("database is locked")})
	if _, err := sw.findCandidates(); err == nil {
		t.Fatal("expected an error when unkeep times can't be read")
	}
	sw.sweepOnce()
	if got := atomic.LoadInt32(deletes); got != 0 {
		t.Fatalf("deleted although unkeep times couldn't be read (%d delete calls)", got)
	}
}

// Unkeeping is per series, so it holds back every one of its seasons.
func TestUnkeep_SeriesUnkeptHoldsBackItsSeasons(t *testing.T) {
	cases := []struct {
		name    string
		unkept  map[string]time.Time
		wantDue bool
	}{
		{"series unkept an hour ago", map[string]time.Time{"sonarr:7": time.Now().Add(-time.Hour)}, false},
		{"a movie with the same ID unkept", map[string]time.Time{"radarr:7": time.Now().Add(-time.Hour)}, true},
		{"never unkept", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sonarr, srv := newFakeSonarr(t, season1())
			sw := newSeasonSweeper(t,
				map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
				[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)}, // past the 24h grace
				srv)
			sw.unkept = fakeUnkept{times: c.unkept}

			candidates, err := sw.findCandidates()
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 || candidates[0].due != c.wantDue {
				t.Fatalf("candidates = %+v, want season 1 with due=%v", candidates, c.wantDue)
			}
			sw.sweepOnce()
			if deleted := len(sonarr.deletedFileIDs) > 0; deleted != c.wantDue {
				t.Errorf("deleted = %v (files %v), want %v", deleted, sonarr.deletedFileIDs, c.wantDue)
			}
		})
	}
}
