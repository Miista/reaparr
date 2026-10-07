package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

func intPtr(i int) *int { return &i }

// playedEpisode builds a played Jellyfin episode of series "jf-series-1".
func playedEpisode(id string, season, episode int) jellyfinItem {
	return jellyfinItem{
		ID: id, Name: id, Type: "Episode",
		SeriesID: "jf-series-1", SeriesName: "Some Show",
		ParentIndexNumber: intPtr(season), IndexNumber: intPtr(episode),
		UserData: jellyfinUserData{Played: true},
	}
}

// fakeSonarr serves one series (id 7, tvdb 410092) with the given episodes
// and records every write it receives.
type fakeSonarr struct {
	mu                sync.Mutex
	deletedFileIDs    []int
	unmonitoredEpIDs  []int
	seasonPuts        int
	deletedSeries     bool
	hardlinksDisabled bool
	tags              []arrTag // GET /api/v3/tag
	seriesTags        []int    // tags on series 7
	tagsFail          bool     // GET /api/v3/tag returns 500
	editorBody        map[string]any
}

func newFakeSonarr(t *testing.T, episodes []sonarrEpisode) (*fakeSonarr, *httptest.Server) {
	t.Helper()
	f := &fakeSonarr{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/config/mediamanagement":
			json.NewEncoder(w).Encode(mediaManagementConfig{CopyUsingHardlinks: !f.hardlinksDisabled})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/tag":
			if f.tagsFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(f.tags)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/tag":
			var t arrTag
			json.NewDecoder(r.Body).Decode(&t)
			t.ID = 99
			f.tags = append(f.tags, t)
			json.NewEncoder(w).Encode(t)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/series/editor":
			json.NewDecoder(r.Body).Decode(&f.editorBody)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/series":
			json.NewEncoder(w).Encode([]sonarrSeries{{ID: 7, Title: "Some Show", TvdbID: 410092, Tags: f.seriesTags}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episode":
			json.NewEncoder(w).Encode(episodes)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/episode/monitor":
			var body struct {
				EpisodeIDs []int `json:"episodeIds"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.unmonitoredEpIDs = append(f.unmonitoredEpIDs, body.EpisodeIDs...)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/series/7":
			w.Write([]byte(`{"id":7,"seasons":[{"seasonNumber":1,"monitored":true}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/series/7":
			f.seasonPuts++
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/episodefile/bulk":
			var body struct {
				EpisodeFileIDs []int `json:"episodeFileIds"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.deletedFileIDs = append(f.deletedFileIDs, body.EpisodeFileIDs...)
		case r.Method == http.MethodDelete:
			f.deletedSeries = true
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// aired returns a time safely in the past.
func aired() *time.Time {
	t := time.Now().Add(-30 * 24 * time.Hour)
	return &t
}

// season1 is a 2-episode season 1, both aired and downloaded.
func season1() []sonarrEpisode {
	return []sonarrEpisode{
		{ID: 101, SeasonNumber: 1, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 11, AirDateUtc: aired()},
		{ID: 102, SeasonNumber: 1, EpisodeNumber: 2, HasFile: true, EpisodeFileID: 12, AirDateUtc: aired()},
	}
}

func newSeasonSweeper(t *testing.T, users map[string][]jellyfinItem, stops []jellyfinActivityEntry, sonarrSrv *httptest.Server) *sweeper {
	t.Helper()
	var jfUsers []jellyfinUser
	for id := range users {
		jfUsers = append(jfUsers, jellyfinUser{ID: id, Name: id})
	}
	sort.Slice(jfUsers, func(i, j int) bool { return jfUsers[i].ID < jfUsers[j].ID })

	jellyfin := newFakeJellyfin(t, fakeJellyfinConfig{
		users:           jfUsers,
		itemsByUser:     users,
		activityEntries: stops,
		seriesByID: map[string]jellyfinItem{
			"jf-series-1": {ID: "jf-series-1", Type: "Series", ProviderIds: jellyfinProviders{Tvdb: "410092"}},
		},
	})
	arr := &arrClient{sonarrURL: sonarrSrv.URL, sonarrAPIKey: "k", httpClient: sonarrSrv.Client(), log: testLogger(t)}
	return &sweeper{jellyfin: jellyfin, arr: arr, moviesGracePeriod: 24 * time.Hour, tvGracePeriod: 24 * time.Hour, log: testLogger(t)}
}

func stopped(id string, ago time.Duration) jellyfinActivityEntry {
	return jellyfinActivityEntry{Type: "VideoPlaybackStopped", ItemID: id, Date: time.Now().UTC().Add(-ago)}
}

func TestSeason_FullyWatched_DeletesFilesAndUnmonitors_NeverSeries(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e1", 72*time.Hour), stopped("e2", 48*time.Hour)},
		srv)

	sw.sweepOnce()

	if sonarr.deletedSeries {
		t.Fatal("the series itself was deleted — only the season's files may be")
	}
	if got := sonarr.deletedFileIDs; len(got) != 2 || got[0] != 11 || got[1] != 12 {
		t.Errorf("deleted episode files = %v, want [11 12]", got)
	}
	if got := sonarr.unmonitoredEpIDs; len(got) != 2 {
		t.Errorf("unmonitored episodes = %v, want [101 102]", got)
	}
	if sonarr.seasonPuts != 1 {
		t.Errorf("season unmonitor PUTs = %d, want 1", sonarr.seasonPuts)
	}
}

func TestSeason_PartiallyWatched_IsNotDue(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1)}}, // episode 2 unwatched
		[]jellyfinActivityEntry{stopped("e1", 72*time.Hour)},
		srv)

	due, err := sw.findDue()
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("partially watched season listed as due: %+v", due)
	}
	sw.sweepOnce()
	if len(sonarr.deletedFileIDs) != 0 || sonarr.deletedSeries {
		t.Fatal("deleted something from a partially watched season")
	}
}

func TestSeason_UnairedEpisode_IsNotDue(t *testing.T) {
	future := time.Now().Add(7 * 24 * time.Hour)
	episodes := append(season1(), sonarrEpisode{ID: 103, SeasonNumber: 1, EpisodeNumber: 3, AirDateUtc: &future})
	_, srv := newFakeSonarr(t, episodes)
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)

	if due, _ := sw.findDue(); len(due) != 0 {
		t.Fatalf("season still airing listed as due: %+v", due)
	}
}

func TestSeason_EpisodeWithoutFile_IsNotDue(t *testing.T) {
	episodes := append(season1(), sonarrEpisode{ID: 103, SeasonNumber: 1, EpisodeNumber: 3, HasFile: false, AirDateUtc: aired()})
	_, srv := newFakeSonarr(t, episodes)
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)

	if due, _ := sw.findDue(); len(due) != 0 {
		t.Fatalf("season with an undownloaded episode listed as due: %+v", due)
	}
}

// Watched state is per episode across users: two users each watching half
// the season completes it.
func TestSeason_WatchedAcrossUsers_IsDue(t *testing.T) {
	_, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{
			"u1": {playedEpisode("e1", 1, 1)},
			"u2": {playedEpisode("e2", 1, 2)},
		},
		[]jellyfinActivityEntry{stopped("e1", 72*time.Hour), stopped("e2", 48*time.Hour)},
		srv)

	due, err := sw.findDue()
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || !due[0].resolved || due[0].title != "Some Show — Season 1" {
		t.Fatalf("due = %+v, want one resolved row for Some Show — Season 1", due)
	}
}

// A multi-episode file (E01-E02 in one Jellyfin item) covers both episodes,
// and its single file is deleted once.
func TestSeason_MultiEpisodeFile_CoversRange(t *testing.T) {
	episodes := []sonarrEpisode{
		{ID: 101, SeasonNumber: 1, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 11, AirDateUtc: aired()},
		{ID: 102, SeasonNumber: 1, EpisodeNumber: 2, HasFile: true, EpisodeFileID: 11, AirDateUtc: aired()},
	}
	sonarr, srv := newFakeSonarr(t, episodes)
	double := playedEpisode("e1", 1, 1)
	double.IndexNumberEnd = intPtr(2)
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {double}},
		[]jellyfinActivityEntry{stopped("e1", 48*time.Hour)},
		srv)

	sw.sweepOnce()

	if got := sonarr.deletedFileIDs; len(got) != 1 || got[0] != 11 {
		t.Errorf("deleted episode files = %v, want [11] once", got)
	}
}

// The grace period runs from the season's most recent stop: one episode
// stopped long ago doesn't make the season due if another stopped recently.
func TestSeason_GraceUsesLatestEpisodeStop(t *testing.T) {
	_, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e1", 72*time.Hour), stopped("e2", 2*time.Hour)}, // grace is 24h
		srv)

	if due, _ := sw.findDue(); len(due) != 0 {
		t.Fatalf("season within grace of its latest episode stop listed as due: %+v", due)
	}
}

// If unmonitoring fails, no files may be deleted.
func TestDeleteSeason_UnmonitorFailure_DeletesNothing(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	sw := &sweeper{arr: &arrClient{sonarrURL: srv.URL, sonarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)}, log: testLogger(t)}

	err := sw.deleteSeason(&seasonTarget{seriesID: 7, seasonNumber: 1, episodeIDs: []int{101}, episodeFileIDs: []int{11}})
	if err == nil {
		t.Fatal("expected an error when unmonitoring fails")
	}
	if deletes != 0 {
		t.Fatalf("made %d delete calls after unmonitoring failed, want 0", deletes)
	}
}

// A season that can't be checked against Sonarr is still listed, with the
// reason, so the dashboard shows why it isn't deletable.
func TestSeason_SonarrUnsafe_ListedWithReason(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sonarr.hardlinksDisabled = true
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)

	due, err := sw.findDue()
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].resolved || due[0].reason == "" {
		t.Fatalf("due = %+v, want one unresolved row with a reason", due)
	}
}

// A fully watched season still within its grace period is listed for the
// dashboard (due=false) but never returned by findDue, so nothing deletes it.
func TestSeason_WithinGrace_IsCandidateButNotDue(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 2*time.Hour)}, // grace is 24h
		srv)

	candidates, err := sw.findCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].due || !candidates[0].resolved {
		t.Fatalf("candidates = %+v, want one resolved, not-yet-due season", candidates)
	}
	if due, _ := sw.findDue(); len(due) != 0 {
		t.Fatalf("findDue returned a season still within grace: %+v", due)
	}
	sw.sweepOnce()
	if len(sonarr.deletedFileIDs) != 0 {
		t.Fatalf("deleted %v from a season still within grace", sonarr.deletedFileIDs)
	}
}

// A partially watched season isn't a candidate at all — not even "waiting".
func TestSeason_PartiallyWatched_IsNotCandidate(t *testing.T) {
	_, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1)}},
		[]jellyfinActivityEntry{stopped("e1", 2*time.Hour)},
		srv)

	if candidates, _ := sw.findCandidates(); len(candidates) != 0 {
		t.Fatalf("partially watched season listed: %+v", candidates)
	}
}

// A manual delete ignores the grace period: findOneCandidate finds a fully
// watched season that stopped playing minutes ago, and deleting it works.
func TestManualDelete_IgnoresGracePeriod(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 5*time.Minute)}, // grace is 24h
		srv)

	d, ok, err := sw.findOneCandidate(seasonItemID("jf-series-1", 1))
	if err != nil || !ok {
		t.Fatalf("findOneCandidate = ok %v, err %v; want the in-grace season", ok, err)
	}
	if err := sw.deleteDueItem(d); err != nil {
		t.Fatal(err)
	}
	if len(sonarr.deletedFileIDs) != 2 {
		t.Fatalf("deleted %v, want both episode files", sonarr.deletedFileIDs)
	}
}

// A series carrying the keep tag is never a candidate, so neither the sweep
// nor a manual delete can touch any of its seasons.
func TestKeeper_SeriesTagged_IsNeverDeleted(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sonarr.tags = []arrTag{{ID: 5, Label: "reaparr-keep"}}
	sonarr.seriesTags = []int{5}
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)
	sw.keepTag = "reaparr-keep"

	if candidates, _ := sw.findCandidates(); len(candidates) != 0 {
		t.Fatalf("kept series listed: %+v", candidates)
	}
	if _, ok, _ := sw.findOneCandidate(seasonItemID("jf-series-1", 1)); ok {
		t.Fatal("kept season can be found for a manual delete")
	}
	sw.sweepOnce()
	if len(sonarr.deletedFileIDs) != 0 {
		t.Fatalf("deleted %v from a kept series", sonarr.deletedFileIDs)
	}
}

// If the keep tag can't be read, nothing may be deleted — a keeper must
// never be lost just because Sonarr was briefly unreachable.
func TestKeeper_TagLookupFails_DeletesNothing(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sonarr.tagsFail = true
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)
	sw.keepTag = "reaparr-keep"

	if _, err := sw.findCandidates(); err == nil {
		t.Fatal("expected an error when the keep tag can't be read")
	}
	sw.sweepOnce()
	if len(sonarr.deletedFileIDs) != 0 {
		t.Fatalf("deleted %v although keepers couldn't be determined", sonarr.deletedFileIDs)
	}
}

// Keeping a season creates the tag if needed and tags the whole series.
func TestKeeper_KeepSeason_TagsSeries(t *testing.T) {
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)
	sw.keepTag = "reaparr-keep"

	d, ok, err := sw.findOneCandidate(seasonItemID("jf-series-1", 1))
	if err != nil || !ok {
		t.Fatalf("findOneCandidate = ok %v, err %v", ok, err)
	}
	if err := sw.keep(d); err != nil {
		t.Fatal(err)
	}
	if len(sonarr.tags) != 1 || sonarr.tags[0].Label != "reaparr-keep" {
		t.Fatalf("tags = %+v, want reaparr-keep created", sonarr.tags)
	}
	body := sonarr.editorBody
	if body["applyTags"] != "add" || body["seriesIds"].([]any)[0] != float64(7) || body["tags"].([]any)[0] != float64(99) {
		t.Fatalf("series editor body = %v, want series 7 tagged with 99", body)
	}
}

// Keeping several seasons of one series tags that series once, in one
// bulk call.
func TestKeeper_KeepMany_DedupesSeries(t *testing.T) {
	episodes := append(season1(),
		sonarrEpisode{ID: 201, SeasonNumber: 2, EpisodeNumber: 1, HasFile: true, EpisodeFileID: 21, AirDateUtc: aired()},
	)
	sonarr, srv := newFakeSonarr(t, episodes)
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2), playedEpisode("e3", 2, 1)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour), stopped("e3", 48*time.Hour)},
		srv)
	sw.keepTag = "reaparr-keep"

	candidates, err := sw.findCandidates()
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %+v, err %v; want both seasons", candidates, err)
	}
	tagged, err := sw.keepMany(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if tagged != 1 {
		t.Errorf("tagged = %d, want 1 (one series)", tagged)
	}
	if ids := sonarr.editorBody["seriesIds"].([]any); len(ids) != 1 || ids[0] != float64(7) {
		t.Errorf("seriesIds = %v, want [7]", ids)
	}
}
