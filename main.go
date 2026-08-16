// reaparr: polls Jellyfin on a cron schedule for played items,
// and after a configurable grace period, unmonitors + deletes the
// corresponding Radarr/Sonarr item. Never touches qBittorrent — see
// plan.md. No HTTP surface — this is a background
// process only.
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

	store, err := openStore(cfg.StorePath)
	if err != nil {
		logger.Error("failed to open store", "error", err)
		os.Exit(1)
	}

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
		store:       store,
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
