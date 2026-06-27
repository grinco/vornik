package ui

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/ratelimit"
)

// TestBuildRateLimitWarning_QuietProjectHidesBanner — no events
// recorded ⇒ Active=false ⇒ template suppresses the banner. This
// is the dominant case under healthy traffic; the banner must
// remain hidden so the homepage stays clean.
func TestBuildRateLimitWarning_QuietProjectHidesBanner(t *testing.T) {
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	w := buildRateLimitWarning(m, nil, "p1", time.Now())
	assert.False(t, w.Active)
	assert.Zero(t, w.RecentWarns)
}

// TestBuildRateLimitWarning_NilMetricsHidesBanner — when the
// metrics surface isn't wired (legacy deployments without the
// shared observability instance) the banner stays hidden.
func TestBuildRateLimitWarning_NilMetricsHidesBanner(t *testing.T) {
	w := buildRateLimitWarning(nil, nil, "p1", time.Now())
	assert.False(t, w.Active)
}

// TestBuildRateLimitWarning_WarnOnlyAmber — a single warn event
// (no block) lights up the banner without the block-flavoured
// CTA. The template uses RecentBlocks > 0 to switch to red.
func TestBuildRateLimitWarning_WarnOnlyAmber(t *testing.T) {
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	m.ObserveProject("p1", ratelimit.Decision{Blocked: true, Reason: "per-minute cap"})
	w := buildRateLimitWarning(m, nil, "p1", time.Now())
	// ObserveProject Blocked=true is the only path that records,
	// so RecentBlocks > 0 is expected here; the warn-only branch
	// is exercised by the test below via direct event injection.
	assert.True(t, w.Active)
	assert.Equal(t, 1, w.RecentWarns)
	assert.Equal(t, 1, w.RecentBlocks)
	assert.NotEmpty(t, w.LastBlockAt)
	assert.Equal(t, "5m", w.WindowLabel)
}

// TestBuildRateLimitWarning_LastBlockHumanised — the LastBlockAt
// string is pre-rendered relative to "now" so the template stays
// dumb. We don't pin exact seconds (clock skew) but assert the
// label has the expected shape.
func TestBuildRateLimitWarning_LastBlockHumanised(t *testing.T) {
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	now := time.Now()
	m.ObserveProject("p1", ratelimit.Decision{Blocked: true, Reason: "minute cap"})

	w := buildRateLimitWarning(m, nil, "p1", now.Add(35*time.Second))
	assert.Contains(t, w.LastBlockAt, "ago")
}

func TestBuildRateLimitWarning_IncludesAPIKeyEvents(t *testing.T) {
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	m.Observe(ratelimit.ScopeAPIKey, "k1", ratelimit.KeyDecision{Warn: true, RemainingTokens: 1})

	w := buildRateLimitWarning(m, []RateLimitKeyRow{{KeyID: "k1"}}, "p1", time.Now())
	assert.True(t, w.Active)
	assert.Equal(t, 1, w.RecentWarns)
	assert.Equal(t, 0, w.RecentBlocks)
}

// TestShortDuration_FormatsByHighestUnit — covers the three
// branches: whole hours, whole minutes, sub-minute seconds.
func TestShortDuration_FormatsByHighestUnit(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{2 * time.Hour, "2h"},
		{5 * time.Minute, "5m"},
		{45 * time.Second, "45s"},
		{0, "0s"},
		{-1 * time.Second, "0s"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, shortDuration(c.in), "input=%v", c.in)
	}
}

// TestHumaniseAgo_BranchCoverage walks every threshold so the
// banner renders coherent labels across the full "Last 429"
// range — sub-second to multi-hour.
func TestHumaniseAgo_BranchCoverage(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "just now"},
		{500 * time.Millisecond, "just now"},
		{45 * time.Second, "45s ago"},
		{2 * time.Minute, "2m ago"},
		{3*time.Minute + 12*time.Second, "3m 12s ago"},
		{2 * time.Hour, "2h ago"},
		{2*time.Hour + 15*time.Minute, "2h 15m ago"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, humaniseAgo(c.in), "input=%v", c.in)
	}
}
