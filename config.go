package main

import (
	"fmt"
	"os"
	"time"
)

type config struct {
	ListenAddr    string
	DBPath        string
	LogLevel      string
	GracePeriod   time.Duration
	SweepInterval time.Duration

	RadarrURL    string
	RadarrAPIKey string
	SonarrURL    string
	SonarrAPIKey string
}

// logAttrs returns the config as slog fields for the startup log line. API
// keys are redacted to a presence check — never logged in full, even at
// debug level, since these are live credentials for services that can
// delete files.
func (c config) logAttrs() []any {
	return []any{
		"listen_addr", c.ListenAddr,
		"db_path", c.DBPath,
		"log_level", c.LogLevel,
		"grace_period", c.GracePeriod.String(),
		"sweep_interval", c.SweepInterval.String(),
		"radarr_url", c.RadarrURL,
		"radarr_api_key_set", c.RadarrAPIKey != "",
		"sonarr_url", c.SonarrURL,
		"sonarr_api_key_set", c.SonarrAPIKey != "",
	}
}

func loadConfig() (config, error) {
	cfg := config{
		ListenAddr:   envOr("LISTEN_ADDR", ":8080"),
		DBPath:       envOr("DB_PATH", "/data/watch-cleanup.db"),
		LogLevel:     envOr("LOG_LEVEL", "info"),
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
