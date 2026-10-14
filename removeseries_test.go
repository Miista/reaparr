package main

import (
	"strings"
	"testing"
	"time"
)

// watchedSeason1Sweeper is a sweeper over a fully watched, past-grace
// season 1 of Sonarr series 7.
func watchedSeason1Sweeper(t *testing.T) (*fakeSonarr, *sweeper) {
	t.Helper()
	sonarr, srv := newFakeSonarr(t, season1())
	sw := newSeasonSweeper(t,
		map[string][]jellyfinItem{"u1": {playedEpisode("e1", 1, 1), playedEpisode("e2", 1, 2)}},
		[]jellyfinActivityEntry{stopped("e2", 48*time.Hour)},
		srv)
	sw.keepTag = "reaparr-keep"
	return sonarr, sw
}

func TestRemoveEndedSeries_EndedAndEmpty_DeletesSeriesWithFolder(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true
	sw.removeEndedSeries = true

	sw.sweepOnce()

	if len(sonarr.deletedFileIDs) != 2 {
		t.Fatalf("season files were not deleted first: %v", sonarr.deletedFileIDs)
	}
	if !sonarr.deletedSeries {
		t.Fatal("an ended series with no files left was not removed")
	}
	if !strings.Contains(sonarr.seriesDeleteQuery, "deleteFiles=true") || !strings.Contains(sonarr.seriesDeleteQuery, "addImportListExclusion=false") {
		t.Errorf("series delete query = %q, want deleteFiles=true and addImportListExclusion=false", sonarr.seriesDeleteQuery)
	}
}

func TestRemoveEndedSeries_SettingOff_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true

	sw.sweepOnce()

	if sonarr.deletedSeries {
		t.Fatal("the series was removed with the setting off")
	}
}

func TestRemoveEndedSeries_StillAiring_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = false
	sw.removeEndedSeries = true

	sw.sweepOnce()

	if sonarr.deletedSeries {
		t.Fatal("a series that hasn't ended was removed")
	}
}

func TestRemoveEndedSeries_SpecialsStillHaveFiles_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true
	sonarr.specialsFiles = 1
	sw.removeEndedSeries = true

	sw.sweepOnce()

	if sonarr.deletedSeries {
		t.Fatal("a series with a file left in season 0 was removed")
	}
}

func TestRemoveEndedSeries_ReReadFails_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true
	sonarr.seriesReadFails = true
	sw.removeEndedSeries = true

	sw.sweepOnce()

	if len(sonarr.deletedFileIDs) != 2 {
		t.Fatalf("the season delete itself should still succeed: %v", sonarr.deletedFileIDs)
	}
	if sonarr.deletedSeries {
		t.Fatal("the series was removed although it couldn't be re-read")
	}
}

// The season check already skips keepers, but the series is re-read after the
// delete, so a keep tag added in between must still protect it.
func TestRemoveEndedSeries_KeepTagged_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true
	sonarr.tags = []arrTag{{ID: 5, Label: "reaparr-keep"}}
	sonarr.seriesTags = []int{5}

	sw.removeSeriesIfDone("Some Show — Season 1", 7)

	if sonarr.deletedSeries {
		t.Fatal("a keeper was removed")
	}
}

func TestRemoveEndedSeries_TagLookupFails_KeepsSeries(t *testing.T) {
	sonarr, sw := watchedSeason1Sweeper(t)
	sonarr.ended = true
	sonarr.tagsFail = true

	sw.removeSeriesIfDone("Some Show — Season 1", 7)

	if sonarr.deletedSeries {
		t.Fatal("the series was removed although the keep tag couldn't be read")
	}
}
