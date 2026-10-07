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
// It also holds an in-memory preview of every deletion candidate
// (cachedDue), rebuilt by RefreshPreview on its own fixed interval (see
// main.go's runPreviewLoop), independently of the deletion sweep. Both go
// through findCandidates, so the preview and the sweep never disagree on
// what counts — the sweep simply runs its own fresh findDue rather than
// trusting the preview. Nothing is persisted: after a restart the preview
// is rebuilt straight away. The dashboard's GET /api/due reads this cache
// rather than triggering its own live scan on every page load/poll — a
// manual delete, however, always re-verifies that one specific item via
// findOneCandidate before acting (see Delete below), so a stale preview
// can never cause a wrong deletion.
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

// sweep runs one scheduled deletion sweep, gated on the daemon being
// enabled and on the required services being configured — reaparr stays
// up (so they can be configured from the dashboard) but refuses to sweep
// until they are.
func (l *liveSweeper) sweep() {
	s := l.current()
	if !s.daemonEnabled {
		s.log.Info().Msg("daemon is disabled, skipping scheduled sweep — delete from the dashboard instead")
		return
	}
	if missing := s.missingServices(); len(missing) > 0 {
		s.log.Warn().Msg(fmt.Sprintf("unable to run: required services not configured: %s", strings.Join(missing, ", ")))
		return
	}
	s.sweepOnce()
}

// RefreshPreview implements api.Sweeper: rebuilds the dashboard's
// in-memory list via findCandidates — the same matching the sweep uses,
// but including items still within their grace period (due=false) so the
// dashboard shows what's waiting too. Skipped (leaving an empty list) while
// required services are missing — the dashboard's banner explains why.
func (l *liveSweeper) RefreshPreview() {
	s := l.current()

	var due []dueItem
	var err error
	if len(s.missingServices()) == 0 {
		due, err = s.findCandidates()
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
		out = append(out, api.DueItem{
			ID:          d.id,
			Title:       d.title,
			Kind:        string(d.kind),
			GracePeriod: d.gracePeriod.String(),
			StoppedAt:   d.stoppedAt.Format(time.RFC3339),
			DueAt:       d.stoppedAt.Add(d.gracePeriod).Format(time.RFC3339),
			Due:         d.due,
			Resolved:    d.resolved,
			Reason:      d.reason,
		})
	}
	return out
}

// Delete implements api.Sweeper, deleting one specific watched movie or
// fully watched season by its id — right away, regardless of its grace
// period, which only governs the scheduled sweep. Always re-verifies it
// live via findOneCandidate first — never trusts the cached list FindDue
// serves — so a manual delete acts on freshly-confirmed reality even if the
// dashboard's displayed list is behind (see the liveSweeper doc comment).
// If it no longer qualifies (unplayed again since the cache was built),
// that is reported as "nothing to delete", not silently treated as success.
func (l *liveSweeper) Delete(id string) error {
	s := l.current()
	d, ok, err := s.findOneCandidate(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is no longer watched (re-verified just now) — nothing deleted", id)
	}
	if !d.resolved {
		return fmt.Errorf("'%s' can't be deleted: %s", d.title, d.reason)
	}
	if err := s.deleteDueItem(d); err != nil {
		return err
	}
	l.dropFromPreview(id)
	return nil
}

// DeleteSelected implements api.Sweeper: a manual, on-demand delete of the
// given dashboard rows — regardless of grace period (which only governs the
// scheduled sweep) or whether the daemon is enabled — plus the Seerr
// cleanup. Like Delete, it never trusts the cached preview: it runs a fresh
// findCandidates first, and selected items that no longer qualify (e.g.
// unplayed since the list was built) are simply not deleted. The preview
// is rebuilt afterwards.
func (l *liveSweeper) DeleteSelected(ids []string) (api.DeleteResult, error) {
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	l.current().log.Info().Msg(fmt.Sprintf("starting a manual delete of %d selected item(s)", len(ids)))
	return l.deleteCandidates(func(d dueItem) bool { return selected[d.id] })
}

// deleteCandidates re-checks every candidate live, deletes those keep
// accepts, runs the Seerr cleanup, and rebuilds the preview.
func (l *liveSweeper) deleteCandidates(keep func(dueItem) bool) (api.DeleteResult, error) {
	s := l.current()
	if missing := s.missingServices(); len(missing) > 0 {
		return api.DeleteResult{}, fmt.Errorf("required services not configured: %s", strings.Join(missing, ", "))
	}

	candidates, err := s.findCandidates()
	if err != nil {
		return api.DeleteResult{}, err
	}
	var targets []dueItem
	for _, d := range candidates {
		if keep(d) {
			targets = append(targets, d)
		}
	}
	deleted, skipped, failed := s.deleteAllDue(targets)
	l.RefreshPreview()

	return api.DeleteResult{Deleted: deleted, Skipped: skipped, Failed: failed}, nil
}

// DaemonEnabled implements api.Sweeper.
func (l *liveSweeper) DaemonEnabled() bool {
	return l.current().daemonEnabled
}

// NextRun implements api.Sweeper: when the next scheduled sweep fires, per
// the current poll schedule (the same computation runCronLoop uses).
func (l *liveSweeper) NextRun() time.Time {
	return l.current().schedule.Next(time.Now())
}

// dropFromPreview removes one just-deleted item from the cached preview so
// the dashboard doesn't keep listing it until the next refresh.
func (l *liveSweeper) dropFromPreview(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.cachedDue[:0:0]
	for _, d := range l.cachedDue {
		if d.id != id {
			kept = append(kept, d)
		}
	}
	l.cachedDue = kept
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
