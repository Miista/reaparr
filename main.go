// watch-cleanup-tool: listens for Jellyfin "played" webhooks, and after a
// configurable grace period, unmonitors + deletes the corresponding
// Radarr/Sonarr item. Never touches qBittorrent — see watch-cleanup-tool-plan.md.
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
		// Logger isn't built yet (LOG_LEVEL itself may be what's unparsed
		// in a future config field), so fall back to the default logger.
		slog.Error("config error", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	logger.Info("starting watch-cleanup-tool", cfg.logAttrs()...)

	store, err := openStore(cfg.DBPath)
	if err != nil {
		logger.Error("failed to open store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	arr := &arrClient{
		radarrURL:    cfg.RadarrURL,
		radarrAPIKey: cfg.RadarrAPIKey,
		sonarrURL:    cfg.SonarrURL,
		sonarrAPIKey: cfg.SonarrAPIKey,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		log:          logger.With("component", "arr"),
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook/jellyfin", &webhookHandler{store: store, log: logger.With("component", "webhook")})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sweeper := &sweeper{
		store:       store,
		arr:         arr,
		gracePeriod: cfg.GracePeriod,
		interval:    cfg.SweepInterval,
		log:         logger.With("component", "sweep"),
	}
	go sweeper.run(ctx)

	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
