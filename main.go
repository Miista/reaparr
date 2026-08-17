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

	if !waitForHardlinkPrecondition(ctx, arr, logger.With("component", "startup"), time.Hour) {
		logger.Info("shutting down")
		return
	}

	sweeper.run(ctx)
	logger.Info("shutting down")
}

// waitForHardlinkPrecondition blocks (idling, not exiting — see below) until
// every configured service reports copyUsingHardlinks enabled, or ctx is
// cancelled. Returns false if ctx was cancelled first.
//
// Reaparr never exits over this: it's stateless, so a restart after fixing
// the setting always does the fully correct thing regardless of how long
// this sat idle — there's no missed window to worry about, unlike a
// stateful tool where downtime could mean lost events. Idling instead of
// exiting also avoids a Docker restart-loop; `docker ps` shows a normal
// running container, and the fix (enable hardlinks in Radarr/Sonarr) takes
// effect on the next check without needing a restart.
func waitForHardlinkPrecondition(ctx context.Context, arr *arrClient, log *slog.Logger, recheckInterval time.Duration) bool {
	for {
		ok := true

		if arr.hasRadarr() {
			usesHardlinks, err := arr.radarrUsesHardlinks()
			if err != nil {
				log.Error("could not check radarr's hardlink setting, will retry", "error", err)
				ok = false
			} else if !usesHardlinks {
				log.Error("radarr is NOT configured to use hardlinks (copyUsingHardlinks=false) — deleting a file would delete the ONLY copy, breaking any active seed and potentially violating private tracker seed-time/ratio rules. Idling and re-checking periodically until this is fixed. Enable 'Use Hardlinks instead of Copy' in Radarr's Media Management settings.", "recheck_interval", recheckInterval.String())
				ok = false
			}
		}

		if arr.hasSonarr() {
			usesHardlinks, err := arr.sonarrUsesHardlinks()
			if err != nil {
				log.Error("could not check sonarr's hardlink setting, will retry", "error", err)
				ok = false
			} else if !usesHardlinks {
				log.Error("sonarr is NOT configured to use hardlinks (copyUsingHardlinks=false) — deleting a file would delete the ONLY copy, breaking any active seed and potentially violating private tracker seed-time/ratio rules. Idling and re-checking periodically until this is fixed. Enable 'Use Hardlinks instead of Copy' in Sonarr's Media Management settings.", "recheck_interval", recheckInterval.String())
				ok = false
			}
		}

		if ok {
			log.Info("hardlink precondition satisfied, starting normal sweeps")
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(recheckInterval):
		}
	}
}
