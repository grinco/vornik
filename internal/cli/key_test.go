package cli

import (
	"testing"
	"time"
)

// TestParseExpiry_RFC3339 — full timestamp form passes through
// unchanged. Operators who script against the CLI should be able
// to set an exact instant rather than a fuzzy duration.
func TestParseExpiry_RFC3339(t *testing.T) {
	got, err := parseExpiry("2026-12-31T00:00:00Z")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseExpiry_DurationShorthand — `30d`, `6m`, `1y` are the
// affordances humans actually type. The math is "now + N units"
// in wall-clock semantics — using AddDate so DST / month-length
// edge cases land where a human would expect.
func TestParseExpiry_DurationShorthand(t *testing.T) {
	cases := []struct {
		in   string
		unit string
		n    int
	}{
		{"30d", "d", 30},
		{"6m", "m", 6},
		{"1y", "y", 1},
		{"365d", "d", 365},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseExpiry(c.in)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			now := time.Now().UTC()
			var want time.Time
			switch c.unit {
			case "d":
				want = now.AddDate(0, 0, c.n)
			case "m":
				want = now.AddDate(0, c.n, 0)
			case "y":
				want = now.AddDate(c.n, 0, 0)
			}
			// Allow 5s slack: parseExpiry runs `now.UTC()` after the
			// test took its own `now`. Anything beyond that is
			// almost-certainly a real bug.
			diff := got.Sub(want)
			if diff < -5*time.Second || diff > 5*time.Second {
				t.Errorf("got %v, want ~%v (diff %v)", got, want, diff)
			}
		})
	}
}

// TestParseExpiry_RejectsMalformed — operator-facing diagnostics
// must say "I don't understand this" rather than silently default
// to zero-time (which the daemon would treat as "expired forever
// ago"). Two failure shapes: unrecognised unit, unparseable
// number.
func TestParseExpiry_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"30",   // no unit
		"d",    // no number
		"30h",  // unsupported unit
		"-5d",  // negative not allowed
		"0d",   // zero not allowed
		"abcd", // gibberish
		"30 d", // whitespace inside not parsed
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := parseExpiry(c)
			if err == nil {
				t.Errorf("parseExpiry(%q) returned nil err, want non-nil", c)
			}
		})
	}
}
