package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/api"
)

// liveSweeper holds the currently active *sweeper behind a mutex, rebuilt
// from the store's current resolved settings/connections whenever they
// change via the dashboard — so an edit takes effect on the very next
// sweep/preview/manual-delete without a container restart. The cron loop
// (run) always reads through current(), never holding a stale sweeper
// across a reload.
//
// It also holds an in-memory preview of the due list (cachedDue), rebuilt
// by refreshPreview on its own fixed interval (see main.go's
// runPreviewLoop), independently of the deletion sweep. Both call the same
// findDue, so the preview and the sweep never disagree on what counts as
// due — the sweep simply runs its own fresh findDue rather than trusting
// the preview. Nothing is persisted: after a restart the preview is
// rebuilt straight away. The dashboard's GET /api/due reads this cache
// rather than triggering its own live scan on every page load/poll — a
// manual delete, however, always re-verifies that one specific item via
// findOneDue before acting (see Delete below), so a stale preview can
// never cause a wrong deletion.
type liveSweeper struct {
	mu        sync.RWMutex
	cur       *sweeper
	cachedDue []dueItem
	cachedErr error
	cachedAt  time.Time
}

func (l *liveSweeper) set(s *sweeper) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur = s
}

func (l *liveSweeper) current() *sweeper {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cur
}

// sweep runs one deletion sweep, gated on the required services being
// configured — reaparr stays up (so they can be configured from the
// dashboard) but refuses to sweep until they are.
func (l *liveSweeper) sweep() {
	s := l.current()
	if missing := s.missingServices(); len(missing) > 0 {
		s.log.Warn().Msg(fmt.Sprintf("unable to run: required services not configured: %s", strings.Join(missing, ", ")))
		return
	}
	s.sweepOnce()
}

// refreshPreview rebuilds the dashboard's in-memory due list via the same
// findDue the sweep uses. Skipped (leaving an empty list) while required
// services are missing — the dashboard's banner explains why.
func (l *liveSweeper) refreshPreview() {
	s := l.current()

	var due []dueItem
	var err error
	if len(s.missingServices()) == 0 {
		due, err = s.findDue()
	}

	l.mu.Lock()
	l.cachedDue = due
	l.cachedErr = err
	l.cachedAt = time.Now()
	l.mu.Unlock()
}

// MissingServices implements api.Sweeper.
func (l *liveSweeper) MissingServices() []string {
	return l.current().missingServices()
}

// FindDue implements api.Sweeper, returning the cached due list from the
// most recent preview refresh rather than triggering a fresh scan — see the
// liveSweeper doc comment for why that's safe for a read-only preview.
func (l *liveSweeper) FindDue() ([]api.DueItem, error) {
	l.mu.RLock()
	due, err := l.cachedDue, l.cachedErr
	l.mu.RUnlock()

	if err != nil {
		return nil, err
	}
	return renderDue(due), nil
}

// renderDue translates the main package's internal dueItem slice into the
// api package's plain, JSON-ready DueItem rendering.
func renderDue(due []dueItem) []api.DueItem {
	out := make([]api.DueItem, 0, len(due))
	for _, d := range due {
		kind := ""
		if d.resolved {
			kind = string(d.arrItem.kind)
		}
		out = append(out, api.DueItem{
			JellyfinItemID: d.item.ID,
			Title:          displayTitle(d.item),
			Kind:           kind,
			GracePeriod:    d.gracePeriod.String(),
			StoppedAt:      d.stoppedAt.Format(time.RFC3339),
			Resolved:       d.resolved,
		})
	}
	return out
}

// Delete implements api.Sweeper, deleting one specific due item identified
// by its Jellyfin item ID. Always re-verifies that single item live via
// findOneDue first — never trusts the cached list FindDue serves — so a
// manual delete acts on freshly-confirmed reality even if the dashboard's
// displayed list is a sweep or two behind (see the liveSweeper doc
// comment). If the item no longer qualifies (unplayed again, or back
// within its grace period since the cache was built), that is reported as
// "nothing to delete", not silently treated as success.
func (l *liveSweeper) Delete(jellyfinItemID string) error {
	s := l.current()
	d, ok, err := s.findOneDue(jellyfinItemID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("item %s is no longer due for deletion (re-verified just now)", jellyfinItemID)
	}
	if !d.resolved {
		return fmt.Errorf("'%s' has no matching radarr/sonarr entry to delete", displayTitle(d.item))
	}
	return s.deleteDueItem(d)
}

// connectionTester implements api.ConnectionTester using one-off clients
// built from whatever url/apiKey is passed, independent of the live
// sweeper's currently configured clients — so "Test connection" can
// validate a value before it's saved.
type connectionTester struct {
	httpClient *http.Client
	log        zerolog.Logger
}

func (t *connectionTester) TestJellyfin(url, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("an API key is required")
	}
	c := &jellyfinClient{baseURL: url, apiKey: apiKey, httpClient: t.httpClient, log: t.log}
	_, err := c.users()
	return err
}

func (t *connectionTester) TestRadarr(url, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("an API key is required")
	}
	c := &arrClient{radarrURL: url, radarrAPIKey: apiKey, httpClient: t.httpClient, log: t.log}
	_, err := c.radarrUsesHardlinks()
	return err
}

func (t *connectionTester) TestSonarr(url, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("an API key is required")
	}
	c := &arrClient{sonarrURL: url, sonarrAPIKey: apiKey, httpClient: t.httpClient, log: t.log}
	_, err := c.sonarrUsesHardlinks()
	return err
}

func (t *connectionTester) TestSeerr(url, apiKey string) error {
	if apiKey == "" {
		return fmt.Errorf("an API key is required")
	}
	c := &seerrClient{baseURL: url, apiKey: apiKey, httpClient: t.httpClient, log: t.log}
	_, err := c.deletedRequests()
	return err
}
