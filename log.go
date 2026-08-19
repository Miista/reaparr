package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// newLogger builds the process-wide slog.Logger, using compactHandler so
// output reads as plain sentences ("TIME | LEVEL | CATEGORY | message"),
// matching how the other *arr-ecosystem tools in this stack (e.g.
// decluttarr) log — not a big structured key=value dump. Level is
// configurable via LOG_LEVEL since a delete-capable tool needs to be able
// to turn up verbosity live without a redeploy.
//
// Log call sites are expected to build a complete, readable sentence via
// fmt.Sprintf and pass it as the message with no additional attributes —
// any attributes that ARE attached (there are a couple of intentional
// exceptions, e.g. a startup config dump) are appended as trailing
// key=value pairs, not the primary way information is conveyed.
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

	handler := newCompactHandler(os.Stdout, level)
	return slog.New(handler)
}

// compactHandler renders "15:04:05 | LEVEL | category | message key=val ...".
// The category comes from a "component" attribute set via logger.With — see
// main.go, where each subsystem gets its own logger.With("component", "x").
type compactHandler struct {
	w      io.Writer
	level  slog.Level
	mu     *sync.Mutex
	prefix []slog.Attr // accumulated via WithAttrs (e.g. component=sweep)
}

func newCompactHandler(w io.Writer, level slog.Level) *compactHandler {
	return &compactHandler{w: w, level: level, mu: &sync.Mutex{}}
}

func (h *compactHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *compactHandler) Handle(_ context.Context, r slog.Record) error {
	category := "reaparr"
	var extra []string

	for _, a := range h.prefix {
		if a.Key == "component" {
			category = a.Value.String()
			continue
		}
		extra = append(extra, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
	}

	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			category = a.Value.String()
			return true
		}
		extra = append(extra, fmt.Sprintf("%s=%v", a.Key, a.Value.Any()))
		return true
	})

	line := fmt.Sprintf("%s | %-5s | %s | %s", r.Time.Format("15:04:05"), r.Level.String(), category, r.Message)
	if len(extra) > 0 {
		line += " " + strings.Join(extra, " ")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, line)
	return err
}

func (h *compactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &compactHandler{w: h.w, level: h.level, mu: h.mu}
	next.prefix = append(append([]slog.Attr(nil), h.prefix...), attrs...)
	return next
}

func (h *compactHandler) WithGroup(_ string) slog.Handler {
	// Not used anywhere in this codebase — groups would need their own
	// prefixing scheme, not worth building until something actually needs it.
	return h
}
