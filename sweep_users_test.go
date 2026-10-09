package main

import (
	"testing"
	"time"
)

func userFilterFixture(t *testing.T) *jellyfinClient {
	t.Helper()
	played := func(id string) []jellyfinItem {
		return []jellyfinItem{{ID: id, Name: id, Type: "Movie", UserData: jellyfinUserData{Played: true}}}
	}
	return newFakeJellyfin(t, fakeJellyfinConfig{
		users: []jellyfinUser{{ID: "ua", Name: "A"}, {ID: "ub", Name: "B"}, {ID: "uc", Name: "C"}},
		itemsByUser: map[string][]jellyfinItem{
			"ua": played("by-a"), "ub": played("by-b"), "uc": played("by-c"),
		},
	})
}

func playedIDs(items []jellyfinItem) map[string]bool {
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	return ids
}

func TestCurrentlyPlayedItems_OnlyConfiguredUsersCount(t *testing.T) {
	sw := &sweeper{jellyfin: userFilterFixture(t), jellyfinUsers: []string{"a", "UB"}, log: testLogger(t)}

	items, err := sw.currentlyPlayedItems()
	if err != nil {
		t.Fatal(err)
	}
	ids := playedIDs(items)
	if !ids["by-a"] || !ids["by-b"] || ids["by-c"] || len(ids) != 2 {
		t.Fatalf("expected only A's and B's items, got %v", ids)
	}
}

func TestCurrentlyPlayedItems_MatchesByUserID(t *testing.T) {
	sw := &sweeper{jellyfin: userFilterFixture(t), jellyfinUsers: []string{"uc"}, log: testLogger(t)}

	items, err := sw.currentlyPlayedItems()
	if err != nil {
		t.Fatal(err)
	}
	if ids := playedIDs(items); !ids["by-c"] || len(ids) != 1 {
		t.Fatalf("expected only C's item, got %v", ids)
	}
}

func TestCurrentlyPlayedItems_NoFilterMeansAllUsers(t *testing.T) {
	sw := &sweeper{jellyfin: userFilterFixture(t), log: testLogger(t)}

	items, err := sw.currentlyPlayedItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected all 3 users' items, got %d", len(items))
	}
}

func TestCurrentlyPlayedItems_UnknownUsersOnlyErrors(t *testing.T) {
	sw := &sweeper{jellyfin: userFilterFixture(t), jellyfinUsers: []string{"nobody"}, log: testLogger(t)}

	if _, err := sw.currentlyPlayedItems(); err == nil {
		t.Fatal("expected an error rather than a fallback to all users")
	}
}

// An excluded user's playback must not touch an item's timestamp: C's newer
// stop event can't shadow A's, and an item only C stopped has no event.
func TestLatestStopEvents_IgnoresExcludedUsers(t *testing.T) {
	now := time.Now().UTC()
	jf := newFakeJellyfin(t, fakeJellyfinConfig{
		activityEntries: []jellyfinActivityEntry{
			{Type: "VideoPlaybackStopped", ItemID: "shared", UserID: "uc", Date: now.Add(-1 * time.Hour)},
			// Dashed + upper-case, as the activity log may spell /Users' "ua".
			{Type: "VideoPlaybackStopped", ItemID: "shared", UserID: "U-A", Date: now.Add(-48 * time.Hour)},
			{Type: "VideoPlaybackStopped", ItemID: "only-c", UserID: "uc", Date: now.Add(-2 * time.Hour)},
		},
	})

	got, err := jf.latestStopEvents(map[string]bool{normalizeUserID("ua"): true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["shared"].Equal(now.Add(-48*time.Hour)) {
		t.Fatalf("expected only A's event for 'shared', got %v", got)
	}

	all, err := jf.latestStopEvents(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !all["shared"].Equal(now.Add(-1*time.Hour)) {
		t.Fatalf("nil filter should include everyone, got %v", all)
	}
}

func TestParseUserList(t *testing.T) {
	got := parseUserList(" A, ,b ,, C ")
	if len(got) != 3 || got[0] != "A" || got[1] != "b" || got[2] != "C" {
		t.Fatalf("got %q", got)
	}
}
