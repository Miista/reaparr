package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Keepers: a movie (Radarr) or series (Sonarr) carrying the keep tag
// (setting keep_tag, default "reaparr-keep") is never deleted — not by the
// scheduled sweep, nor by a manual delete. The tag lives in Radarr/Sonarr
// themselves, so it's visible and editable there too. In Sonarr a tag
// applies to the whole series, so keeping one season keeps them all.

// arrService names which *arr app a keeper lives in.
type arrService string

const (
	serviceRadarr arrService = "radarr"
	serviceSonarr arrService = "sonarr"
)

type arrTag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

func (a *arrClient) endpoint(svc arrService) (baseURL, apiKey string) {
	if svc == serviceRadarr {
		return a.radarrURL, a.radarrAPIKey
	}
	return a.sonarrURL, a.sonarrAPIKey
}

// findTag looks up a tag by label (Radarr/Sonarr store labels lowercased,
// so the match is case-insensitive). found=false means it doesn't exist.
func (a *arrClient) findTag(svc arrService, label string) (id int, found bool, err error) {
	baseURL, apiKey := a.endpoint(svc)
	var tags []arrTag
	if err := a.get(baseURL+"/api/v3/tag", apiKey, &tags); err != nil {
		return 0, false, err
	}
	for _, t := range tags {
		if strings.EqualFold(t.Label, label) {
			return t.ID, true, nil
		}
	}
	return 0, false, nil
}

// ensureTag returns the tag's ID, creating it if it doesn't exist yet.
func (a *arrClient) ensureTag(svc arrService, label string) (int, error) {
	if id, found, err := a.findTag(svc, label); err != nil || found {
		return id, err
	}
	baseURL, apiKey := a.endpoint(svc)
	var created arrTag
	if err := a.do(http.MethodPost, baseURL+"/api/v3/tag", apiKey, map[string]string{"label": label}, &created); err != nil {
		return 0, fmt.Errorf("creating %s tag %q: %w", svc, label, err)
	}
	return created.ID, nil
}

// setTag adds or removes a tag on movies (Radarr) or series (Sonarr) via
// the bulk editor, which only touches tags.
func (a *arrClient) setTag(svc arrService, ids []int, tagID int, add bool) error {
	baseURL, apiKey := a.endpoint(svc)
	apply := "remove"
	if add {
		apply = "add"
	}
	body := map[string]any{"tags": []int{tagID}, "applyTags": apply}
	path := "/api/v3/movie/editor"
	if svc == serviceRadarr {
		body["movieIds"] = ids
	} else {
		body["seriesIds"] = ids
		path = "/api/v3/series/editor"
	}
	return a.send(http.MethodPut, baseURL+path, apiKey, body)
}

func hasTag(tags []int, id int) bool {
	if id == 0 {
		return false
	}
	for _, t := range tags {
		if t == id {
			return true
		}
	}
	return false
}

// keepTagIDs are the keep tag's IDs in Radarr/Sonarr for one sweep (0 =
// the tag doesn't exist there, so nothing is kept).
type keepTagIDs struct {
	radarr int
	sonarr int
}

// resolveKeepTags looks up the keep tag in each configured service. A
// failed lookup is an error, not "nothing kept": the caller must then not
// delete anything, or a keeper could be deleted just because Radarr/Sonarr
// was briefly unreachable.
func (s *sweeper) resolveKeepTags() (keepTagIDs, error) {
	var ids keepTagIDs
	if s.arr.hasRadarr() {
		id, _, err := s.arr.findTag(serviceRadarr, s.keepTag)
		if err != nil {
			return ids, fmt.Errorf("reading radarr's tags: %w", err)
		}
		ids.radarr = id
	}
	if s.arr.hasSonarr() {
		id, _, err := s.arr.findTag(serviceSonarr, s.keepTag)
		if err != nil {
			return ids, fmt.Errorf("reading sonarr's tags: %w", err)
		}
		ids.sonarr = id
	}
	return ids, nil
}

// keptItem is one movie or series carrying the keep tag.
type keptItem struct {
	service arrService
	id      int
	title   string
}

// keptItems lists everything currently carrying the keep tag, watched or
// not — including anything tagged directly in Radarr/Sonarr.
func (s *sweeper) keptItems() ([]keptItem, error) {
	tags, err := s.resolveKeepTags()
	if err != nil {
		return nil, err
	}
	var out []keptItem
	if tags.radarr != 0 {
		var movies []radarrMovie
		if err := s.arr.get(s.arr.radarrURL+"/api/v3/movie", s.arr.radarrAPIKey, &movies); err != nil {
			return nil, err
		}
		for _, m := range movies {
			if hasTag(m.Tags, tags.radarr) {
				out = append(out, keptItem{service: serviceRadarr, id: m.ID, title: m.Title})
			}
		}
	}
	if tags.sonarr != 0 {
		var series []sonarrSeries
		if err := s.arr.get(s.arr.sonarrURL+"/api/v3/series", s.arr.sonarrAPIKey, &series); err != nil {
			return nil, err
		}
		for _, sr := range series {
			if hasTag(sr.Tags, tags.sonarr) {
				out = append(out, keptItem{service: serviceSonarr, id: sr.ID, title: sr.Title})
			}
		}
	}
	return out, nil
}

// keep tags a due item's movie or series as a keeper.
func (s *sweeper) keep(d dueItem) error {
	if !d.resolved {
		return fmt.Errorf("'%s' isn't matched to radarr/sonarr: %s", d.title, d.reason)
	}
	_, err := s.keepMany([]dueItem{d})
	return err
}

// keepMany tags the movies/series of the given due items as keepers — one
// bulk tag call per service. Unresolved items are skipped; seasons of the
// same series collapse into one series. Returns how many movies/series
// were tagged.
func (s *sweeper) keepMany(items []dueItem) (int, error) {
	ids := map[arrService][]int{}
	seen := map[string]bool{}
	for _, d := range items {
		if !d.resolved {
			continue
		}
		svc, arrID := serviceRadarr, d.movie.ID
		if d.kind == kindSeason {
			svc, arrID = serviceSonarr, d.season.seriesID
		}
		if key := fmt.Sprintf("%s:%d", svc, arrID); !seen[key] {
			seen[key] = true
			ids[svc] = append(ids[svc], arrID)
		}
	}

	var tagged int
	for _, svc := range []arrService{serviceRadarr, serviceSonarr} {
		if len(ids[svc]) == 0 {
			continue
		}
		tagID, err := s.arr.ensureTag(svc, s.keepTag)
		if err != nil {
			return tagged, err
		}
		if err := s.arr.setTag(svc, ids[svc], tagID, true); err != nil {
			return tagged, fmt.Errorf("tagging in %s: %w", svc, err)
		}
		tagged += len(ids[svc])
		s.log.Info().Msg(fmt.Sprintf("marked %d %s item(s) as keepers (tag %q)", len(ids[svc]), svc, s.keepTag))
	}
	return tagged, nil
}

// unkeep removes the keep tag from a movie or series.
func (s *sweeper) unkeep(svc arrService, arrID int) error {
	tagID, found, err := s.arr.findTag(svc, s.keepTag)
	if err != nil || !found {
		return err
	}
	if err := s.arr.setTag(svc, []int{arrID}, tagID, false); err != nil {
		return fmt.Errorf("untagging %s id %d: %w", svc, arrID, err)
	}
	s.log.Info().Msg(fmt.Sprintf("%s id %d is no longer a keeper", svc, arrID))
	return nil
}
