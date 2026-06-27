package ui

import (
	"testing"
	"time"
)

// TestTradingAccountStatus — structured account-block provenance, including
// the hard >24h staleness tier and the no-snapshot vs store-unavailable split.
func TestTradingAccountStatus(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name               string
		enabled, reachable bool
		fb                 snapshotFallback
		asOf               time.Time
		want               string
	}{
		{"disabled", false, false, snapNone, time.Time{}, ""},
		{"live", true, true, snapNone, time.Time{}, "live"},
		{"fresh fallback", true, false, snapApplied, now.Add(-2 * time.Minute), "snapshot_fresh"},
		{"stale fallback (>15m, <=24h)", true, false, snapApplied, now.Add(-2 * time.Hour), "snapshot_stale"},
		{"expired fallback (>24h)", true, false, snapApplied, now.Add(-48 * time.Hour), "snapshot_expired"},
		{"no snapshot exists", true, false, snapNone, time.Time{}, "no_snapshot"},
		{"snapshot store unavailable", true, false, snapUnavailable, time.Time{}, "snapshot_unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tradingAccountStatus(c.enabled, c.reachable, c.fb, c.asOf, now); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
