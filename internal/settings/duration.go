package settings

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MinGracePeriod is the shortest allowed grace period. The grace period
// exists to catch a premature or mistaken "played" flag before the
// scheduled run deletes anything; anything shorter defeats that. Manual
// deletes from the dashboard aren't affected by it.
const MinGracePeriod = 24 * time.Hour

// ParseGracePeriod extends time.ParseDuration with "d" (days) and "w"
// (weeks) suffixes — e.g. "7d", "2w" — since those are unambiguous, fixed
// lengths (unlike "months", which vary and were deliberately left
// unsupported rather than picking an arbitrary fixed-length definition for
// them). Anything without a "d"/"w" suffix is passed straight through to
// time.ParseDuration, so all of its existing forms ("45m", "6h", "1h30m",
// ...) keep working unchanged. It doesn't enforce MinGracePeriod — see
// ValidateGracePeriod.
func ParseGracePeriod(raw string) (time.Duration, error) {
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

// ValidateGracePeriod parses a grace period and enforces MinGracePeriod.
func ValidateGracePeriod(raw string) (time.Duration, error) {
	d, err := ParseGracePeriod(raw)
	if err != nil {
		return 0, err
	}
	if d < MinGracePeriod {
		return d, fmt.Errorf("grace period %q is shorter than the 1 day minimum", raw)
	}
	return d, nil
}
