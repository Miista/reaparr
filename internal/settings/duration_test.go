package settings

import (
	"testing"
	"time"
)

func TestParseGracePeriod(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "2w", want: 2 * 7 * 24 * time.Hour},
		{in: "0.5d", want: 12 * time.Hour},
		{in: "168h", want: 168 * time.Hour},
		{in: "45m", want: 45 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "bogus", wantErr: true},
		{in: "7x", wantErr: true},
		{in: "d", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseGracePeriod(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseGracePeriod(%q): expected error, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGracePeriod(%q): unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ParseGracePeriod(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestValidateGracePeriod_EnforcesOneDayMinimum(t *testing.T) {
	for _, ok := range []string{"1d", "24h", "1.5d", "2w"} {
		if _, err := ValidateGracePeriod(ok); err != nil {
			t.Errorf("ValidateGracePeriod(%q) = %v, want ok", ok, err)
		}
	}
	for _, short := range []string{"0.5d", "23h", "45m", "0s"} {
		if _, err := ValidateGracePeriod(short); err == nil {
			t.Errorf("ValidateGracePeriod(%q) accepted a value below the 1 day minimum", short)
		}
	}
	if _, err := ValidateGracePeriod("bogus"); err == nil {
		t.Error("ValidateGracePeriod accepted an unparseable value")
	}
}
