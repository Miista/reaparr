package main

import (
	"net/http"
	"testing"
)

func TestKeptState(t *testing.T) {
	arr, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/tag":
			_, _ = w.Write([]byte(`[{"id":5,"label":"reaparr-keep"}]`))
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id":1,"tags":[5]},{"id":2,"tags":[]}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	sw := &sweeper{arr: arr, keepTag: "reaparr-keep", log: testLogger(t)}

	got, err := sw.keptState([]libraryItem{
		{service: serviceRadarr, id: 1},
		{service: serviceRadarr, id: 2},
		{service: serviceRadarr, id: 99},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got[libraryItem{service: serviceRadarr, id: 1}] {
		t.Error("movie 1 carries the tag, want kept")
	}
	if kept, found := got[libraryItem{service: serviceRadarr, id: 2}]; !found || kept {
		t.Errorf("movie 2 is in the library without the tag: found=%v kept=%v, want found and not kept", found, kept)
	}
	if _, found := got[libraryItem{service: serviceRadarr, id: 99}]; found {
		t.Error("movie 99 is not in the library, so it must be left out rather than reported as not kept")
	}
}

func TestKeptStateFailsForAServiceThatIsNotConfigured(t *testing.T) {
	arr, _ := newTestArrClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should be requested, got %s", r.URL.Path)
	})
	arr.sonarrAPIKey = ""
	sw := &sweeper{arr: arr, keepTag: "reaparr-keep", log: testLogger(t)}

	if _, err := sw.keptState([]libraryItem{{service: serviceSonarr, id: 1}}); err == nil {
		t.Fatal("a title of an unconfigured service must be an error, not \"not kept\"")
	}
}
