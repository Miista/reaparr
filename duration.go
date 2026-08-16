package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseGracePeriod extends time.ParseDuration with "d" (days) and "w"
// (weeks) suffixes — e.g. "7d", "2w" — since those are unambiguous, fixed
// lengths (unlike "months", which vary and were deliberately left
// unsupported rather than picking an arbitrary fixed-length definition for
// them). Anything without a "d"/"w" suffix is passed straight through to
// time.ParseDuration, so all of its existing forms ("45m", "6h", "1h30m",
// ...) keep working unchanged.
func parseGracePeriod(raw string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day count %q: %w", raw, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}

	if n, ok := strings.CutSuffix(raw, "w"); ok {
		weeks, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid week count %q: %w", raw, err)
		}
		return time.Duration(weeks * 7 * 24 * float64(time.Hour)), nil
	}

	return time.ParseDuration(raw)
}
