package main

import (
	"io"
	"os"
	"testing"

	"github.com/rs/zerolog"
)

// testLogger discards output by default so `go test` stays quiet; run with
// `go test -v` to see log lines interleaved with test output (useful when
// debugging a failing case).
func testLogger(t *testing.T) zerolog.Logger {
	t.Helper()
	var w io.Writer = io.Discard
	if testing.Verbose() {
		w = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
	}
	return zerolog.New(w).Level(zerolog.DebugLevel).With().Timestamp().Logger()
}
