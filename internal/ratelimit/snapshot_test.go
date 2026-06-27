package ratelimit

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLimiterSnapshot_PerProjectCounts asserts Snapshot returns the
// trailing-minute + trailing-hour counts per project without mutating
// the underlying log (a follow-up Check should see the same state).
func TestLimiterSnapshot_PerProjectCounts(t *testing.T) {
	l := New()
	now := time.Now()
	// p1: 2 inside the minute, plus 1 inside the hour but outside
	// the minute. p2: 1 inside the minute. p3: only ancient entries
	// — must not appear in the result (Snapshot filters scopes
	// whose log is entirely outside the hour window).
	l.Record("p1", now.Add(-30*time.Second))
	l.Record("p1", now.Add(-10*time.Second))
	l.Record("p1", now.Add(-20*time.Minute))
	l.Record("p2", now.Add(-5*time.Second))
	l.Record("p3", now.Add(-90*time.Minute))

	snap := l.Snapshot(now)
	got := make(map[string]ProjectSnapshot)
	for _, s := range snap {
		got[s.ProjectID] = s
	}
	require.Contains(t, got, "p1")
	require.Contains(t, got, "p2")
	assert.NotContains(t, got, "p3", "ancient-only projects must be filtered out")
	assert.Equal(t, 2, got["p1"].MinuteCount)
	assert.Equal(t, 3, got["p1"].HourCount)
	assert.Equal(t, 1, got["p2"].MinuteCount)
	assert.Equal(t, 1, got["p2"].HourCount)
}

// TestLimiterSnapshotFor_AbsentProjectReportedFalse verifies the
// "seen vs unseen" boolean discrimination so the UI can render
// explicit "0 tasks this hour" badges only for projects that have
// any history at all.
func TestLimiterSnapshotFor_AbsentProjectReportedFalse(t *testing.T) {
	l := New()
	_, ok := l.SnapshotFor("never-seen", time.Now())
	assert.False(t, ok)

	l.Record("seen", time.Now())
	s, ok := l.SnapshotFor("seen", time.Now())
	assert.True(t, ok)
	assert.Equal(t, "seen", s.ProjectID)
	assert.Equal(t, 1, s.MinuteCount)
}

// TestLimiterSnapshot_NilSafe ensures Snapshot on a nil receiver
// (the "no limiter configured" path) returns nil rather than
// panicking — production wires this through service container
// constructors where the limiter may not be enabled.
func TestLimiterSnapshot_NilSafe(t *testing.T) {
	var l *Limiter
	assert.Nil(t, l.Snapshot(time.Now()))
	_, ok := l.SnapshotFor("anything", time.Now())
	assert.False(t, ok)
}

// TestAPIKeyLimiterSnapshot_ReadOnlyCurrentBucket verifies
// Snapshot does NOT consume a token (the operator-panel reader
// must not skew the bucket level it's reporting).
func TestAPIKeyLimiterSnapshot_ReadOnlyCurrentBucket(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Now()
	// burst 10, consume 3 — bucket lands at 7.
	for i := 0; i < 3; i++ {
		l.Allow("k1", 100, 10, now)
	}
	snap := l.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "k1", snap[0].KeyID)
	assert.InDelta(t, 7.0, snap[0].Tokens, 0.001)

	// A second Snapshot must show the same level — no consumption
	// happened in between.
	snap2 := l.Snapshot()
	require.Len(t, snap2, 1)
	assert.InDelta(t, 7.0, snap2[0].Tokens, 0.001)
}

// TestAPIKeyLimiterSnapshotFor_UnseenKeyReportedFalse mirrors the
// per-key SnapshotFor contract — distinguishes "key bucket never
// allocated" from "allocated and empty" so the UI can hide
// allocations that haven't seen traffic yet.
func TestAPIKeyLimiterSnapshotFor_UnseenKeyReportedFalse(t *testing.T) {
	l := NewAPIKeyLimiter()
	_, ok := l.SnapshotFor("never")
	assert.False(t, ok)

	l.Allow("seen", 10, 5, time.Now())
	s, ok := l.SnapshotFor("seen")
	assert.True(t, ok)
	assert.Equal(t, "seen", s.KeyID)
	assert.InDelta(t, 4.0, s.Tokens, 0.001) // burst 5, consumed 1
}

// TestAPIKeyLimiterSnapshot_NilSafe — nil receiver returns nil
// rather than panicking; matches the project-limiter contract.
func TestAPIKeyLimiterSnapshot_NilSafe(t *testing.T) {
	var l *APIKeyLimiter
	assert.Nil(t, l.Snapshot())
	_, ok := l.SnapshotFor("k")
	assert.False(t, ok)
}

// TestMetrics_StatusFor_WindowedWarnAndBlockCounts checks that the
// 5-minute recent-warn + recent-block + last-block timestamps
// surface correctly and that events older than the window are
// pruned out on read.
func TestMetrics_StatusFor_WindowedWarnAndBlockCounts(t *testing.T) {
	m := NewMetrics(newTestRegistry())
	base := time.Now()
	clock := base
	m.eventClock = func() time.Time { return clock }

	// Two warns + one block inside the window, one ancient warn
	// that must be pruned.
	clock = base.Add(-10 * time.Minute)
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Warn: true})
	clock = base.Add(-3 * time.Minute)
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Warn: true})
	clock = base.Add(-2 * time.Minute)
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Warn: true})
	clock = base.Add(-1 * time.Minute)
	m.Observe(ScopeAPIKey, "k1", KeyDecision{Blocked: true, Warn: true})

	clock = base
	s := m.StatusFor(ScopeAPIKey, "k1")
	assert.Equal(t, 3, s.RecentWarns, "ancient warn must be pruned")
	assert.Equal(t, 1, s.RecentBlocks)
	assert.WithinDuration(t, base.Add(-1*time.Minute), s.LastBlockAt, time.Second)
}

// TestMetrics_StatusFor_EmptyWhenAllowOnly — Observe with the
// Allow outcome must NOT record an event; the homepage banner
// stays hidden under healthy traffic.
func TestMetrics_StatusFor_EmptyWhenAllowOnly(t *testing.T) {
	m := NewMetrics(newTestRegistry())
	m.Observe(ScopeAPIKey, "k1", KeyDecision{RemainingTokens: 9})
	s := m.StatusFor(ScopeAPIKey, "k1")
	assert.Zero(t, s.RecentWarns)
	assert.Zero(t, s.RecentBlocks)
	assert.True(t, s.LastBlockAt.IsZero())
}

// TestMetrics_StatusFor_NilAndEmptyArgs — nil receiver, empty
// scope, or empty id all return the zero summary. The status
// endpoint relies on this for "ratelimit metrics not wired" or
// "no key for this project" paths.
func TestMetrics_StatusFor_NilAndEmptyArgs(t *testing.T) {
	var m *Metrics
	assert.Equal(t, StatusSummary{}, m.StatusFor(ScopeAPIKey, "k1"))

	live := NewMetrics(newTestRegistry())
	assert.Equal(t, StatusSummary{}, live.StatusFor("", "k1"))
	assert.Equal(t, StatusSummary{}, live.StatusFor(ScopeAPIKey, ""))
}

// TestMetrics_StatusFor_RingCapped — more than eventRingCap warns
// land in the ring; the oldest are dropped so memory stays bounded.
// We assert by warning thousands of times in a tight window and
// confirming the recent-warn count never exceeds the cap.
func TestMetrics_StatusFor_RingCapped(t *testing.T) {
	m := NewMetrics(newTestRegistry())
	base := time.Now()
	clock := base
	m.eventClock = func() time.Time { return clock }

	for i := 0; i < eventRingCap*3; i++ {
		clock = base.Add(time.Duration(i) * time.Microsecond)
		m.Observe(ScopeAPIKey, "hot", KeyDecision{Warn: true})
	}
	clock = base.Add(time.Second)
	s := m.StatusFor(ScopeAPIKey, "hot")
	assert.LessOrEqual(t, s.RecentWarns, eventRingCap)
}

// TestMetrics_StatusFor_GCsEmptyRings — once every event in a
// scope's ring slides outside the window, the entry is removed
// from the map so an idle scope doesn't squat memory forever.
func TestMetrics_StatusFor_GCsEmptyRings(t *testing.T) {
	m := NewMetrics(newTestRegistry())
	base := time.Now()
	clock := base
	m.eventClock = func() time.Time { return clock }

	clock = base.Add(-30 * time.Minute)
	m.Observe(ScopeAPIKey, "cold", KeyDecision{Warn: true})
	clock = base
	s := m.StatusFor(ScopeAPIKey, "cold")
	assert.Zero(t, s.RecentWarns)
	// Internal: the map entry should be gone.
	m.eventsMu.Lock()
	_, present := m.events[eventKey{scope: ScopeAPIKey, id: "cold"}]
	m.eventsMu.Unlock()
	assert.False(t, present)
}

// TestMetrics_ObserveProject_RecordsBlockEvent — the per-project
// limiter routes block decisions into the same event ring under
// ScopeProject so the homepage banner reflects task-creation
// throttling as well as per-key throttling.
func TestMetrics_ObserveProject_RecordsBlockEvent(t *testing.T) {
	m := NewMetrics(newTestRegistry())
	m.ObserveProject("p1", Decision{Blocked: true, Reason: "minute cap"})
	s := m.StatusFor(ScopeProject, "p1")
	assert.Equal(t, 1, s.RecentWarns)
	assert.Equal(t, 1, s.RecentBlocks)
	assert.False(t, s.LastBlockAt.IsZero())
}

// newTestRegistry returns a fresh prometheus.Registerer so test
// runs don't collide on the default registry. Internal helper to
// keep the test bodies focused on the ratelimit surface.
func newTestRegistry() prometheus.Registerer { return prometheus.NewRegistry() }
