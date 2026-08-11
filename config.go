package main

import (
	"fmt"
	"os"
	"time"
)

type config struct {
	ListenAddr    string
	DBPath        string
	GracePeriod   time.Duration
	SweepInterval time.Duration

	RadarrURL    string
	RadarrAPIKey string
	SonarrURL    string
	SonarrAPIKey string
}

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		DBPath:       envOr("DB_PATH", "/data/watch-cleanup.db"),
		RadarrURL:    envOr("RADARR_URL", "http://radarr:7878"),
		SonarrURL:    envOr("SONARR_URL", "http://sonarr:8989"),
		RadarrAPIKey: os.Getenv("RADARR_API_KEY"),
		SonarrAPIKey: os.Getenv("SONARR_API_KEY"),
	}

	graceDays := envOr("GRACE_PERIOD_DAYS", "7")
	var days int
	if _, err := fmt.Sscanf(graceDays, "%d", &days); err != nil {
		return cfg, fmt.Errorf("invalid GRACE_PERIOD_DAYS %q: %w", graceDays, err)
	}
	cfg.GracePeriod = time.Duration(days) * 24 * time.Hour

	sweepMinutes := envOr("SWEEP_INTERVAL_MINUTES", "60")
	var minutes int
	if _, err := fmt.Sscanf(sweepMinutes, "%d", &minutes); err != nil {
		return cfg, fmt.Errorf("invalid SWEEP_INTERVAL_MINUTES %q: %w", sweepMinutes, err)
	}
	cfg.SweepInterval = time.Duration(minutes) * time.Minute

	if cfg.RadarrAPIKey == "" {
		return cfg, fmt.Errorf("RADARR_API_KEY is required")
	}
	if cfg.SonarrAPIKey == "" {
		return cfg, fmt.Errorf("SONARR_API_KEY is required")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
