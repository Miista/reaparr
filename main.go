// watch-cleanup-tool: listens for Jellyfin "played" webhooks, and after a
// configurable grace period, unmonitors + deletes the corresponding
// Radarr/Sonarr item. Never touches qBittorrent — see watch-cleanup-tool-plan.md.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := openStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	arr := &arrClient{
		radarrURL:    cfg.RadarrURL,
		radarrAPIKey: cfg.RadarrAPIKey,
		sonarrURL:    cfg.SonarrURL,
		sonarrAPIKey: cfg.SonarrAPIKey,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook/jellyfin", &webhookHandler{store: store})
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
	}
	go sweeper.run(ctx)

	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
