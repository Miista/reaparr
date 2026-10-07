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
	"fmt"
	"net/http"
	"os"
	"os/signal"
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

// staticDir is where the Dockerfile copies the dashboard assets — see
// internal main's noCache comment in automouse for why this isn't a build
// step away from the plain source tree.
const staticDir = "web/static"

func main() {
	dataDir := envOr("REAPARR_DATA_DIR", "/app/data")

	st, err := store.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open state store: %v\n", err)
		os.Exit(1)
	}

	// The very first boot ever (fresh data dir, nothing persisted yet)
	// seeds the store from whatever env vars are set, so an
	// env-vars-only deployment (the only mode reaparr supported before
	// this dashboard existed) keeps working with zero config changes.
	// Subsequent boots leave persisted values alone — env vars still win
	// per-field via settings.Resolve/ResolveConnections either way, this
	// seeding only matters for fields with no env var set at all, so
	// their first-ever dashboard view shows the old defaults rather than
	// store.DefaultSettings()'s bare fallbacks.
	seedFromEnvOnFirstBoot(st)

	logLevel := settings.Resolve(currentSettings(st)).Settings.LogLevel
	logger := newLogger(logLevel)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	live := &liveSweeper{}
	reload := func() { live.set(buildSweeper(st, httpClient, logger)) }
	reload()

	tester := &connectionTester{httpClient: httpClient, log: withComponent(logger, "test-connection")}

	apiServer := api.New(st, live, tester, reload, logger)
	mux := http.NewServeMux()
	apiServer.Routes(mux)
	mux.Handle("/", noCache(http.FileServer(http.Dir(staticDir))))

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

	<-ctx.Done()
	logger.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// runCronLoop runs sweeps on the schedule currently in effect, via
// liveSweeper.sweepAndCache rather than sweeper.sweepOnce directly, so each
// tick's single findDue scan serves both the deletion pass and the
// dashboard's cached preview (see adapter.go). Unlike the original
// sweeper.run, this re-reads the schedule from the store before computing
// each "next" fire time, so a dashboard edit to the poll schedule takes
// effect without a restart — the previous sweeper.run (used in tests via a
// fixed schedule) is left unchanged for that purpose.
func runCronLoop(ctx context.Context, live *liveSweeper, st *store.Store, logger zerolog.Logger) {
	live.sweepAndCache()

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
			live.sweepAndCache()
		}
	}
}

// buildSweeper constructs a fresh *sweeper from the store's current
// resolved settings/connections — called at startup and again after every
// settings/connections save (see main's reload closure).
func buildSweeper(st *store.Store, httpClient *http.Client, logger zerolog.Logger) *sweeper {
	resolvedSettings := settings.Resolve(currentSettings(st))
	resolvedConnections := settings.ResolveConnections(currentConnections(st))

	cfg := resolvedSettings.Settings
	conns := resolvedConnections.Connections

	moviesGrace, err := parseGracePeriod(cfg.MoviesGracePeriod)
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("invalid movies grace period %q, falling back to 7d: %v", cfg.MoviesGracePeriod, err))
		moviesGrace = 7 * 24 * time.Hour
	}
	tvGrace, err := parseGracePeriod(cfg.TVGracePeriod)
	if err != nil {
		logger.Error().Msg(fmt.Sprintf("invalid tv grace period %q, falling back to 7d: %v", cfg.TVGracePeriod, err))
		tvGrace = 7 * 24 * time.Hour
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

// seedFromEnvOnFirstBoot writes env-sourced values into the store exactly
// once — detected by the data dir having no prior config.json before
// store.Open created a fresh default one (store.Open always leaves exactly
// DefaultSettings()/DefaultConnections() in that case, so this just
// confirms the store already matches the hardcoded defaults byte-for-byte
// before overwriting, i.e. it's a fresh install, not a deliberate reset by
// the user back to defaults).
func seedFromEnvOnFirstBoot(st *store.Store) {
	_ = st.Update(func(state *store.State) {
		if state.Settings == store.DefaultSettings() {
			state.Settings = store.Settings{
				LogLevel:          envOr("LOG_LEVEL", store.DefaultSettings().LogLevel),
				PollSchedule:      envOr("POLL_SCHEDULE", store.DefaultSettings().PollSchedule),
				MoviesGracePeriod: envOr("DELETE_MOVIES_AFTER", store.DefaultSettings().MoviesGracePeriod),
				TVGracePeriod:     envOr("DELETE_TV_SHOWS_AFTER", store.DefaultSettings().TVGracePeriod),
			}
		}
		if state.Connections == store.DefaultConnections() {
			state.Connections = store.Connections{
				Jellyfin: store.Connection{
					URL:    envOr("JELLYFIN_URL", store.DefaultConnections().Jellyfin.URL),
					APIKey: os.Getenv("JELLYFIN_API_KEY"),
				},
				Radarr: store.Connection{
					URL:    envOr("RADARR_URL", store.DefaultConnections().Radarr.URL),
					APIKey: os.Getenv("RADARR_API_KEY"),
				},
				Sonarr: store.Connection{
					URL:    envOr("SONARR_URL", store.DefaultConnections().Sonarr.URL),
					APIKey: os.Getenv("SONARR_API_KEY"),
				},
				Seerr: store.Connection{
					URL:    envOr("SEERR_URL", store.DefaultConnections().Seerr.URL),
					APIKey: os.Getenv("SEERR_API_KEY"),
				},
			}
		}
	})
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
