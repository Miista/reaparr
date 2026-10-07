package main

import (
	"fmt"
	"io"
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

// libraryItem is one movie (Radarr) or series (Sonarr), with whether it
// carries the keep tag.
type libraryItem struct {
	service arrService
	id      int
	title   string
	year    int
	kept    bool
}

// libraryItems lists every movie and series in Radarr/Sonarr, live — the
// dashboard's Library tab, where anything can be kept before it's watched.
func (s *sweeper) libraryItems() ([]libraryItem, error) {
	tags, err := s.resolveKeepTags()
	if err != nil {
		return nil, err
	}
	var out []libraryItem
	if s.arr.hasRadarr() {
		var movies []radarrMovie
		if err := s.arr.get(s.arr.radarrURL+"/api/v3/movie", s.arr.radarrAPIKey, &movies); err != nil {
			return nil, fmt.Errorf("reading radarr's movies: %w", err)
		}
		for _, m := range movies {
			out = append(out, libraryItem{service: serviceRadarr, id: m.ID, title: m.Title, year: m.Year, kept: hasTag(m.Tags, tags.radarr)})
		}
	}
	if s.arr.hasSonarr() {
		var series []sonarrSeries
		if err := s.arr.get(s.arr.sonarrURL+"/api/v3/series", s.arr.sonarrAPIKey, &series); err != nil {
			return nil, fmt.Errorf("reading sonarr's series: %w", err)
		}
		for _, sr := range series {
			out = append(out, libraryItem{service: serviceSonarr, id: sr.ID, title: sr.Title, year: sr.Year, kept: hasTag(sr.Tags, tags.sonarr)})
		}
	}
	return out, nil
}

// setKept adds (keep=true) or removes the keep tag on Radarr movies /
// Sonarr series by their own IDs — one bulk call per service.
func (s *sweeper) setKept(ids map[arrService][]int, keep bool) error {
	for _, svc := range []arrService{serviceRadarr, serviceSonarr} {
		if len(ids[svc]) == 0 {
			continue
		}
		var tagID int
		if keep {
			id, err := s.arr.ensureTag(svc, s.keepTag)
			if err != nil {
				return err
			}
			tagID = id
		} else {
			id, found, err := s.arr.findTag(svc, s.keepTag)
			if err != nil {
				return err
			}
			if !found {
				continue // nothing can carry a tag that doesn't exist
			}
			tagID = id
		}
		if err := s.arr.setTag(svc, ids[svc], tagID, keep); err != nil {
			return fmt.Errorf("updating keepers in %s: %w", svc, err)
		}
		verb := "marked as keepers"
		if !keep {
			verb = "no longer keepers"
		}
		s.log.Info().Msg(fmt.Sprintf("%d %s item(s) %s (tag %q)", len(ids[svc]), svc, verb, s.keepTag))
	}
	return nil
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

	if err := s.setKept(ids, true); err != nil {
		return 0, err
	}
	return len(ids[serviceRadarr]) + len(ids[serviceSonarr]), nil
}

// poster fetches a movie's / series' small poster from Radarr/Sonarr's own
// media cover cache (never from the internet), for the dashboard. The
// caller must close the body.
func (a *arrClient) poster(svc arrService, id int) (io.ReadCloser, string, error) {
	baseURL, apiKey := a.endpoint(svc)
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v3/mediacover/%d/poster-250.jpg", baseURL, id), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Api-Key", apiKey)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, "", fmt.Errorf("%s poster for %d: %s", svc, id, resp.Status)
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}
