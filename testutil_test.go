package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// testLogger discards output by default so `go test` stays quiet; run with
// `go test -v` to see log lines interleaved with test output (useful when
// debugging a failing case).
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	w := io.Discard
	if testing.Verbose() {
		w = os.Stderr
	}
	return slog.New(newCompactHandler(w, slog.LevelDebug))
}
