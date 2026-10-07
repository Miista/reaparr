package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// A disabled daemon must not touch any service on its scheduled sweep,
// even with everything configured.
func TestSweep_DaemonDisabled_MakesNoRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	live := &liveSweeper{}
	live.set(&sweeper{
		jellyfin:      &jellyfinClient{baseURL: srv.URL, apiKey: "k", httpClient: srv.Client(), log: testLogger(t)},
		arr:           &arrClient{radarrURL: srv.URL, radarrAPIKey: "k", httpClient: srv.Client(), log: testLogger(t)},
		seerr:         &seerrClient{baseURL: srv.URL, apiKey: "k", httpClient: srv.Client(), log: testLogger(t)},
		daemonEnabled: false,
		log:           testLogger(t),
	})

	live.sweep()

	if n := hits.Load(); n != 0 {
		t.Errorf("disabled daemon made %d requests, want 0", n)
	}
}

func TestMissingServices(t *testing.T) {
	configured := func() *sweeper {
		return &sweeper{
			jellyfin: &jellyfinClient{baseURL: "http://jf", apiKey: "k"},
			arr:      &arrClient{radarrURL: "http://r", radarrAPIKey: "k", sonarrURL: "http://s", sonarrAPIKey: "k"},
			seerr:    &seerrClient{baseURL: "http://se", apiKey: "k"},
		}
	}

	tests := []struct {
		name   string
		mutate func(s *sweeper)
		want   []string
	}{
		{"all configured", func(s *sweeper) {}, nil},
		{"radarr only is enough", func(s *sweeper) { s.arr.sonarrAPIKey = "" }, nil},
		{"sonarr only is enough", func(s *sweeper) { s.arr.radarrURL = "" }, nil},
		{"no arr", func(s *sweeper) { s.arr.radarrAPIKey = ""; s.arr.sonarrURL = "" }, []string{"Radarr or Sonarr"}},
		{"jellyfin without key", func(s *sweeper) { s.jellyfin.apiKey = "" }, []string{"Jellyfin"}},
		{"seerr without url", func(s *sweeper) { s.seerr.baseURL = "" }, []string{"Seerr"}},
		{"nothing configured", func(s *sweeper) {
			s.jellyfin.apiKey, s.arr.radarrAPIKey, s.arr.sonarrAPIKey, s.seerr.apiKey = "", "", "", ""
		}, []string{"Jellyfin", "Radarr or Sonarr", "Seerr"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := configured()
			tt.mutate(s)
			if got := s.missingServices(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("missingServices() = %v, want %v", got, tt.want)
			}
		})
	}
}
