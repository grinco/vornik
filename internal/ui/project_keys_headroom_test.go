// Package ui: tests for the per-API-key rate-limit headroom
// columns added to /ui/projects/<id>/keys. Verifies the
// renderKeyRows pure function pins nominal cap shape from the
// persisted row + that renderKeyRowsWithLimiter overlays the live
// bucket level when a limiter is wired.
package ui

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
)

// TestRenderKeyRows_NoRateLimit — when the persisted row has no
// rps/burst set, RateLimited is false and the headroom columns
// render as "—". Pre-batch behaviour (no rate-limit column) stays
// unchanged for unlimited keys.
func TestRenderKeyRows_NoRateLimit(t *testing.T) {
	now := time.Now()
	rows := renderKeyRows([]*persistence.APIKey{
		{
			ID:        "k1",
			Name:      "no-limit-key",
			KeyPrefix: "sk-vornik-p.ab12",
			CreatedAt: now,
		},
	})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].RateLimited {
		t.Error("RateLimited should be false when row has no rps/burst")
	}
	if rows[0].TokensRemaining != "—" {
		t.Errorf("TokensRemaining = %q, want '—'", rows[0].TokensRemaining)
	}
}

// TestRenderKeyRows_WithRateLimit — non-zero rps/burst on the
// persisted row populates the cap columns. TokensRemaining stays
// "—" in the pure path (no limiter wired).
func TestRenderKeyRows_WithRateLimit(t *testing.T) {
	rps, burst := 10, 50
	rows := renderKeyRows([]*persistence.APIKey{
		{
			ID:             "k1",
			Name:           "limited-key",
			KeyPrefix:      "sk-vornik-p.cd34",
			CreatedAt:      time.Now(),
			RateLimitRPS:   &rps,
			RateLimitBurst: &burst,
		},
	})
	if !rows[0].RateLimited {
		t.Error("RateLimited should be true with non-zero rps + burst")
	}
	if rows[0].RateLimitRPS != 10 || rows[0].RateLimitBurst != 50 {
		t.Errorf("rps/burst mismatch: %+v", rows[0])
	}
	// Pure path doesn't consult a limiter; headroom stays "—".
	if rows[0].TokensRemaining != "—" {
		t.Errorf("pure path: TokensRemaining = %q, want '—'", rows[0].TokensRemaining)
	}
}

// TestRenderKeyRowsWithLimiter_BucketAllocated — the wrapper overlays
// live bucket headroom when the limiter has seen the key. Format is
// "X.X / burst"; LastRefillAgo gets a relative-time label.
func TestRenderKeyRowsWithLimiter_BucketAllocated(t *testing.T) {
	rps, burst := 10, 50
	limiter := ratelimit.NewAPIKeyLimiter()
	// Allocate the bucket via an Allow call so the limiter knows
	// about the key; lazy-allocation pattern matches production
	// where AuthMiddleware drives the first Allow on key use.
	limiter.Allow("k1", rps, burst, time.Now())

	srv := NewServer(WithAPIKeyLimiter(limiter))
	rows := srv.renderKeyRowsWithLimiter([]*persistence.APIKey{
		{
			ID:             "k1",
			Name:           "limited-key",
			KeyPrefix:      "sk-vornik-p.cd34",
			CreatedAt:      time.Now(),
			RateLimitRPS:   &rps,
			RateLimitBurst: &burst,
		},
	})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !strings.Contains(rows[0].TokensRemaining, "/ 50") {
		t.Errorf("TokensRemaining missing '/ 50': %q", rows[0].TokensRemaining)
	}
	if rows[0].LastRefillAgo == "—" {
		t.Errorf("LastRefillAgo should be relative-time, got '—'")
	}
}

// TestRenderKeyRowsWithLimiter_BucketNotAllocated — limit
// configured BUT no Allow call yet → render "burst / burst (idle)"
// so the operator sees "limit configured, no traffic yet" rather
// than an alarming zero.
func TestRenderKeyRowsWithLimiter_BucketNotAllocated(t *testing.T) {
	rps, burst := 5, 25
	limiter := ratelimit.NewAPIKeyLimiter()
	srv := NewServer(WithAPIKeyLimiter(limiter))
	rows := srv.renderKeyRowsWithLimiter([]*persistence.APIKey{
		{
			ID:             "k1",
			Name:           "idle-limited-key",
			KeyPrefix:      "sk-vornik-p.ef56",
			CreatedAt:      time.Now(),
			RateLimitRPS:   &rps,
			RateLimitBurst: &burst,
		},
	})
	if !strings.Contains(rows[0].TokensRemaining, "idle") {
		t.Errorf("expected 'idle' marker; got %q", rows[0].TokensRemaining)
	}
}

// TestRelativeShort — pin the small relative-time renderer used in
// the headroom column's "refilled X ago" tooltip. Covers each
// branch so a regression in the breakpoint logic (sub-1s, < min,
// < hour, < day, days) doesn't silently produce a misleading label.
func TestRelativeShort(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Millisecond * 500, "<1s ago"},
		{time.Second * 5, "5s ago"},
		{time.Second * 75, "1m 15s ago"},
		{time.Hour * 3, "3h ago"},
		{time.Hour * 36, "1d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := relativeShort(tc.d)
			if got != tc.want {
				t.Errorf("relativeShort(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}
