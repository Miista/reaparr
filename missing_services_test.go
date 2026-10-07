package main

import (
	"reflect"
	"testing"
)

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
