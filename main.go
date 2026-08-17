// reaparr: on a cron schedule, checks Jellyfin's activity log and current
// watched state to find fully-played titles whose grace period has
// elapsed, then deletes them via Radarr/Sonarr. Never touches qBittorrent
// or Jellyfin's own library. Entirely stateless: every sweep is a fresh,
// complete pass with nothing remembered from the last one, and no HTTP
// surface — this is a background process only.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// Logger isn't built yet (LOG_LEVEL itself may be what's unparsed),
		// so fall back to the default logger.
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("starting reaparr", cfg.logAttrs()...)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	jellyfin := &jellyfinClient{
		baseURL:    cfg.JellyfinURL,
		apiKey:     cfg.JellyfinAPIKey,
		httpClient: httpClient,
		log:        logger.With("component", "jellyfin"),
	}

	arr := &arrClient{
		radarrURL:    cfg.RadarrURL,
		radarrAPIKey: cfg.RadarrAPIKey,
		sonarrURL:    cfg.SonarrURL,
		sonarrAPIKey: cfg.SonarrAPIKey,
		httpClient:   httpClient,
		log:          logger.With("component", "arr"),
	}

	sweeper := &sweeper{
		jellyfin:    jellyfin,
		arr:         arr,
		gracePeriod: cfg.GracePeriod,
		schedule:    cfg.PollSchedule,
		log:         logger.With("component", "sweep"),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sweeper.run(ctx)
	logger.Info("shutting down")
}
