package main

import (
	"fmt"
	"os"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

type config struct {
	LogLevel string

	// Separate grace periods for movies and TV — a household might want a
	// short one for movies (single-sitting watches) and a longer one for
	// TV (a season pack might sit half-watched for a while between
	// episodes).
	MoviesGracePeriod time.Duration
	TVGracePeriod     time.Duration

	PollScheduleRaw string
	PollSchedule    cron.Schedule

	JellyfinURL    string
	JellyfinAPIKey string

	RadarrURL    string
	RadarrAPIKey string
	SonarrURL    string
	SonarrAPIKey string

	// Seerr is entirely optional — if unset, Reaparr never talks to it at
	// all. When configured, Reaparr cleans up stale Seerr requests left
	// behind by Seerr's own "Media Availability Sync" job, which correctly
	// marks a deleted title's media record as DELETED but does not clean
	// up the associated request record(s) — a real, confirmed gap.
	SeerrURL    string
	SeerrAPIKey string
}

// logFields attaches the config as fields on a zerolog event, for the
// startup log line. API keys are redacted to a presence check — never
// logged in full, even at debug level, since these are live credentials
// for services that can delete files.
func (c config) logFields(e *zerolog.Event) *zerolog.Event {
	return e.
		Str("log_level", c.LogLevel).
		Str("movies_grace_period", c.MoviesGracePeriod.String()).
		Str("tv_grace_period", c.TVGracePeriod.String()).
		Str("poll_schedule", c.PollScheduleRaw).
		Str("jellyfin_url", c.JellyfinURL).
		Bool("jellyfin_api_key_set", c.JellyfinAPIKey != "").
		Str("radarr_url", c.RadarrURL).
		Bool("radarr_api_key_set", c.RadarrAPIKey != "").
		Str("sonarr_url", c.SonarrURL).
		Bool("sonarr_api_key_set", c.SonarrAPIKey != "").
		Bool("seerr_configured", c.SeerrAPIKey != "")
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
		SeerrURL:        envOr("SEERR_URL", "http://seerr:5055"),
		SeerrAPIKey:     os.Getenv("SEERR_API_KEY"),
	}

	// A Go duration string extended with "d"/"w" suffixes (see
	// parseGracePeriod) — e.g. "7d", "168h", "45m" are all valid. Accepted
	// this way (rather than a cron expression like POLL_SCHEDULE) because a
	// grace period is a span of time from a reference point, not a
	// recurring schedule — cron expressions don't express "wait this
	// long", only "run at these moments". Named DELETE_*_AFTER (plain
	// language, "delete once this much time has passed since watching")
	// rather than GRACE_PERIOD_* — the grace-period concept is explained
	// in the README, but the env var itself should read clearly even to
	// someone who hasn't read that yet.
	moviesGraceRaw := envOr("DELETE_MOVIES_AFTER", "7d")
	moviesGrace, err := parseGracePeriod(moviesGraceRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid DELETE_MOVIES_AFTER %q: %w", moviesGraceRaw, err)
	}
	cfg.MoviesGracePeriod = moviesGrace

	tvGraceRaw := envOr("DELETE_TV_SHOWS_AFTER", "7d")
	tvGrace, err := parseGracePeriod(tvGraceRaw)
	if err != nil {
		return cfg, fmt.Errorf("invalid DELETE_TV_SHOWS_AFTER %q: %w", tvGraceRaw, err)
	}
	cfg.TVGracePeriod = tvGrace

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
