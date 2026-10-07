// reaparr: on a cron schedule, checks Jellyfin's activity log and current
// watched state to find fully-played titles whose grace period has
// elapsed, then deletes them via Radarr/Sonarr. Never touches qBittorrent
// or Jellyfin's own library.
//
// Settings and connections (Jellyfin/Radarr/Sonarr/Seerr URL + API key) can
// come from either an environment variable or the dashboard — an env var,
// when set, always wins and the dashboard renders that field read-only
// (see internal/settings). Whichever source is in effect, reaparr re-reads
// the current resolved configuration before every sweep, so a dashboard
// edit takes effect on the very next sweep without a restart.
//
// The sweep itself is entirely stateless: every sweep independently
// re-derives everything it needs to know from live Jellyfin data, rather
// than remembering anything about a prior sweep (see sweep.go).
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // embed tzdata so TZ resolves without OS packages (distroless has none)

	"github.com/rs/zerolog"

	"github.com/Miista/reaparr/internal/api"
	"github.com/Miista/reaparr/internal/settings"
	"github.com/Miista/reaparr/internal/store"
)

// addr is the dashboard's bind address. automouse uses :8765 and packrat
// uses :8766 elsewhere in this stack; :8767 is the next free port in that
// range.
const addr = ":8767"

// staticFiles is the dashboard (HTML, JS, CSS, fonts, icon), compiled into
// the binary so reaparr ships as a single file with nothing to copy
// alongside it.
//
//go:embed web/static
var staticFiles embed.FS

// dataDir is where config.json is persisted inside the container — mount a
// volume here to keep dashboard-entered settings across restarts.
const dataDir = "/app/data"

func main() {
	st, err := store.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open state store: %v\n", err)
		os.Exit(1)
	}

	// Env vars are never copied into the store: every read merges env
	// (which wins per field) over persisted values via
	// settings.Resolve/ResolveConnections, so the store only ever holds
	// what was entered in the dashboard.
	logLevel := settings.Resolve(currentSettings(st)).Settings.LogLevel
	logger := newLogger(logLevel)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	live := &liveSweeper{}
	reload := func() { live.set(buildSweeper(st, httpClient, logger)) }
	reload()

	tester := &connectionTester{httpClient: httpClient, log: withComponent(logger, "test-connection")}

	// A dashboard save also refreshes the preview right away, so the due
	// list and the missing-services banner reflect the new configuration
	// without waiting for the next preview tick.
	reloadAndRefresh := func() {
		reload()
		go live.RefreshPreview()
	}

	apiServer := api.New(st, live, tester, reloadAndRefresh, logger)
	mux := http.NewServeMux()
	apiServer.Routes(mux)
	static, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		logger.Fatal().Err(err).Msg("embedded dashboard assets missing")
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(static))))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info().Str("addr", addr).Str("data_dir", dataDir).Msg("starting reaparr dashboard")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("dashboard server stopped")
		}
	}()

	go runCronLoop(ctx, live, st, logger)
	go runPreviewLoop(ctx, live)

	<-ctx.Done()
	logger.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// previewRefreshInterval is how often the dashboard's in-memory due list
// is rebuilt. Deliberately not configurable for now.
const previewRefreshInterval = 15 * time.Minute

// runPreviewLoop rebuilds the dashboard's due-list preview immediately on
// startup and then every previewRefreshInterval, independently of the
// deletion sweep's schedule (see liveSweeper.RefreshPreview).
func runPreviewLoop(ctx context.Context, live *liveSweeper) {
	live.RefreshPreview()

	ticker := time.NewTicker(previewRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			live.RefreshPreview()
		}
	}
}

// runCronLoop runs sweeps on the schedule currently in effect, via
// liveSweeper.sweep rather than sweeper.sweepOnce directly, so each sweep
// is gated on the required services being configured (see adapter.go).
// Unlike the original sweeper.run, this re-reads the schedule from the
// store before computing each "next" fire time, so a dashboard edit to the
// poll schedule takes effect without a restart — the previous sweeper.run
// (used in tests via a fixed schedule) is left unchanged for that purpose.
func runCronLoop(ctx context.Context, live *liveSweeper, st *store.Store, logger zerolog.Logger) {
	live.sweep()

	for {
		schedule := live.current().schedule
		now := time.Now()
		next := schedule.Next(now)
		timer := time.NewTimer(time.Until(next))

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			live.sweep()
		}
	}
}

// gracePeriodOrFallback resolves a grace period setting. An unparseable
// value falls back to 7 days; one below settings.MinGracePeriod (possible
// only via env var — the dashboard and API reject it) is raised to the
// minimum rather than ignored. Either way it's logged as an error.
func gracePeriodOrFallback(name, raw string, logger zerolog.Logger) time.Duration {
	d, err := settings.ParseGracePeriod(raw)
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("invalid %s grace period %q, falling back to 7d: %v", name, raw, err))
		return 7 * 24 * time.Hour
	}
	if d < settings.MinGracePeriod {
		logger.Error().Msg(fmt.Sprintf("%s grace period %q is below the 1 day minimum, using 1d", name, raw))
		return settings.MinGracePeriod
	}
	return d
}

// buildSweeper constructs a fresh *sweeper from the store's current
// resolved settings/connections — called at startup and again after every
// settings/connections save (see main's reload closure).
func buildSweeper(st *store.Store, httpClient *http.Client, logger zerolog.Logger) *sweeper {
	resolvedSettings := settings.Resolve(currentSettings(st))
	resolvedConnections := settings.ResolveConnections(currentConnections(st))

	cfg := resolvedSettings.Settings
	conns := resolvedConnections.Connections

	moviesGrace := gracePeriodOrFallback("movies", cfg.MoviesGracePeriod, logger)
	tvGrace := gracePeriodOrFallback("tv", cfg.TVGracePeriod, logger)
	keepTag := strings.TrimSpace(cfg.KeepTag)
	if keepTag == "" {
		keepTag = store.DefaultSettings().KeepTag
	}
	schedule, err := cronParser.Parse(cfg.PollSchedule)
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("invalid poll schedule %q, falling back to @hourly: %v", cfg.PollSchedule, err))
		schedule, _ = cronParser.Parse("@hourly")
	}

	jellyfin := &jellyfinClient{
		baseURL:    conns.Jellyfin.URL,
		apiKey:     conns.Jellyfin.APIKey,
		httpClient: httpClient,
		log:        withComponent(logger, "jellyfin"),
	}
	arr := &arrClient{
		radarrURL:    conns.Radarr.URL,
		radarrAPIKey: conns.Radarr.APIKey,
		sonarrURL:    conns.Sonarr.URL,
		sonarrAPIKey: conns.Sonarr.APIKey,
		httpClient:   httpClient,
		log:          withComponent(logger, "arr"),
	}
	seerr := &seerrClient{
		baseURL:    conns.Seerr.URL,
		apiKey:     conns.Seerr.APIKey,
		httpClient: httpClient,
		log:        withComponent(logger, "seerr"),
	}

	return &sweeper{
		jellyfin:          jellyfin,
		arr:               arr,
		seerr:             seerr,
		moviesGracePeriod: moviesGrace,
		tvGracePeriod:     tvGrace,
		schedule:          schedule,
		daemonEnabled:     cfg.DaemonEnabled,
		keepTag:           keepTag,
		log:               withComponent(logger, "sweep"),
	}
}

func currentSettings(st *store.Store) store.Settings {
	var s store.Settings
	st.View(func(state store.State) { s = state.Settings })
	return s
}

func currentConnections(st *store.Store) store.Connections {
	var c store.Connections
	st.View(func(state store.State) { c = state.Connections })
	return c
}

// noCache forces revalidation on every static asset request instead of
// letting a browser cache app.js/style.css/index.html indefinitely across
// a deploy — same rationale and implementation as automouse's noCache.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
