package main

import (
	"testing"
	"time"
)

func clearArrEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JELLYFIN_API_KEY", "jf-key")
	t.Setenv("RADARR_API_KEY", "")
	t.Setenv("SONARR_API_KEY", "")
}

func TestLoadConfig_RequiresJellyfinAPIKey(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("JELLYFIN_API_KEY", "")
	t.Setenv("RADARR_API_KEY", "radarr-key")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error when JELLYFIN_API_KEY is missing, got nil")
	}
}

func TestLoadConfig_RejectsNeitherRadarrNorSonarr(t *testing.T) {
	clearArrEnv(t)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected an error when neither RADARR_API_KEY nor SONARR_API_KEY is set, got nil")
	}
}

// A movies-only household has no use for Sonarr, and vice versa — either
// alone must be a valid, working configuration, not just both together.
func TestLoadConfig_AcceptsRadarrOnly(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected radarr-only config to be valid, got error: %v", err)
	}
	if cfg.RadarrAPIKey != "radarr-key" || cfg.SonarrAPIKey != "" {
		t.Fatalf("unexpected config: radarr=%q sonarr=%q", cfg.RadarrAPIKey, cfg.SonarrAPIKey)
	}
}

func TestLoadConfig_AcceptsSonarrOnly(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("SONARR_API_KEY", "sonarr-key")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected sonarr-only config to be valid, got error: %v", err)
	}
	if cfg.SonarrAPIKey != "sonarr-key" || cfg.RadarrAPIKey != "" {
		t.Fatalf("unexpected config: radarr=%q sonarr=%q", cfg.RadarrAPIKey, cfg.SonarrAPIKey)
	}
}

func TestLoadConfig_AcceptsBothRadarrAndSonarr(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")
	t.Setenv("SONARR_API_KEY", "sonarr-key")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected both-configured config to be valid, got error: %v", err)
	}
	if cfg.RadarrAPIKey != "radarr-key" || cfg.SonarrAPIKey != "sonarr-key" {
		t.Fatalf("unexpected config: radarr=%q sonarr=%q", cfg.RadarrAPIKey, cfg.SonarrAPIKey)
	}
}

func TestLoadConfig_GracePeriodsDefaultToSevenDays(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")
	t.Setenv("GRACE_PERIOD_MOVIES", "")
	t.Setenv("GRACE_PERIOD_TV", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MoviesGracePeriod != 7*24*time.Hour {
		t.Fatalf("expected default 7d movies grace period, got %v", cfg.MoviesGracePeriod)
	}
	if cfg.TVGracePeriod != 7*24*time.Hour {
		t.Fatalf("expected default 7d TV grace period, got %v", cfg.TVGracePeriod)
	}
}

// The whole point of the feature: movies and TV must be configurable
// independently, e.g. a short grace period for movies and a longer one
// for TV shows.
func TestLoadConfig_GracePeriodsAreIndependentlyConfigurable(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")
	t.Setenv("GRACE_PERIOD_MOVIES", "2d")
	t.Setenv("GRACE_PERIOD_TV", "7d")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.MoviesGracePeriod != 2*24*time.Hour {
		t.Fatalf("expected 2d movies grace period, got %v", cfg.MoviesGracePeriod)
	}
	if cfg.TVGracePeriod != 7*24*time.Hour {
		t.Fatalf("expected 7d TV grace period, got %v", cfg.TVGracePeriod)
	}
}

func TestLoadConfig_RejectsInvalidGracePeriodMovies(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")
	t.Setenv("GRACE_PERIOD_MOVIES", "bogus")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error for an invalid GRACE_PERIOD_MOVIES value, got nil")
	}
}

func TestLoadConfig_RejectsInvalidGracePeriodTV(t *testing.T) {
	clearArrEnv(t)
	t.Setenv("RADARR_API_KEY", "radarr-key")
	t.Setenv("GRACE_PERIOD_TV", "bogus")

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error for an invalid GRACE_PERIOD_TV value, got nil")
	}
}
