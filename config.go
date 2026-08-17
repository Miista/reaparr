package main

import (
	"fmt"
	"os"
	"time"

	"github.com/robfig/cron/v3"
)

type config struct {
	LogLevel    string
	GracePeriod time.Duration

	PollScheduleRaw string
	PollSchedule    cron.Schedule

	JellyfinURL    string
	JellyfinAPIKey string

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
		"log_level", c.LogLevel,
		"grace_period", c.GracePeriod.String(),
		"poll_schedule", c.PollScheduleRaw,
		"jellyfin_url", c.JellyfinURL,
		"jellyfin_api_key_set", c.JellyfinAPIKey != "",
		"radarr_url", c.RadarrURL,
		"radarr_api_key_set", c.RadarrAPIKey != "",
		"sonarr_url", c.SonarrURL,
		"sonarr_api_key_set", c.SonarrAPIKey != "",
	}
}

// cronParser accepts both standard 5-field cron expressions and the
// "@hourly"/"@daily"/etc descriptors — POLL_SCHEDULE is meant to be set by
// a human, and the descriptors are the readable common case.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func loadConfig() (config, error) {
	cfg := config{
		LogLevel:        envOr("LOG_LEVEL", "info"),
		PollScheduleRaw: envOr("POLL_SCHEDULE", "@hourly"),
		JellyfinURL:     envOr("JELLYFIN_URL", "http://jellyfin:8096"),
		JellyfinAPIKey:  os.Getenv("JELLYFIN_API_KEY"),
		RadarrURL:       envOr("RADARR_URL", "http://radarr:7878"),
		SonarrURL:       envOr("SONARR_URL", "http://sonarr:8989"),
		RadarrAPIKey:    os.Getenv("RADARR_API_KEY"),
		SonarrAPIKey:    os.Getenv("SONARR_API_KEY"),
	}

	// A Go duration string extended with "d"/"w" suffixes (see
	// parseGracePeriod) — e.g. "7d", "168h", "45m" are all valid. Accepted
	// this way (rather than a cron expression like POLL_SCHEDULE) because a
	// grace period is a span of time from a reference point, not a
	// recurring schedule — cron expressions don't express "wait this
	// long", only "run at these moments".
	graceRaw := envOr("GRACE_PERIOD", "7d")
	grace, err := parseGracePeriod(graceRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid GRACE_PERIOD %q: %w", graceRaw, err)
	}
	cfg.GracePeriod = grace

	schedule, err := cronParser.Parse(cfg.PollScheduleRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid POLL_SCHEDULE %q: %w", cfg.PollScheduleRaw, err)
	}
	cfg.PollSchedule = schedule

	if cfg.JellyfinAPIKey == "" {
		return cfg, fmt.Errorf("JELLYFIN_API_KEY is required")
	}
	// At least one of Radarr/Sonarr must be configured, but not
	// necessarily both — a movies-only household has no use for Sonarr,
	// and vice versa. Either alone is a legitimate deployment.
	if cfg.RadarrAPIKey == "" && cfg.SonarrAPIKey == "" {
		return cfg, fmt.Errorf("at least one of RADARR_API_KEY or SONARR_API_KEY is required")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
