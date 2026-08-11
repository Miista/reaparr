package main

import (
	"log/slog"
	"os"
	"strings"
)

// newLogger builds the process-wide slog.Logger. Text handler (not JSON) so
// `docker logs` stays directly readable while keeping key=value fields
// greppable. Level is configurable via LOG_LEVEL since a delete-capable
// tool needs to be able to turn up verbosity live without a redeploy.
func newLogger(levelStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
