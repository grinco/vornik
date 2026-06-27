package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/registry"
)

func TestCheck_NoLimitsConfigured(t *testing.T) {
	l := New()
	p := &registry.Project{ID: "p"}
	now := time.Now()

	// Record many tasks; no caps → never blocked.
	for i := 0; i < 100; i++ {
		l.Record(p.ID, now)
	}
	d := l.Check(p, now)
	assert.False(t, d.Blocked)
}

func TestCheck_PerMinuteBlocks(t *testing.T) {
	l := New()
	p := &registry.Project{ID: "p", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 3}}
	now := time.Now()

	for i := 0; i < 3; i++ {
		l.Record(p.ID, now.Add(time.Duration(i)*time.Second))
	}
	d := l.Check(p, now.Add(4*time.Second))
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "minute")
	assert.Equal(t, 3, d.MinuteCount)
}

func TestCheck_MinuteBoundarySlides(t *testing.T) {
	l := New()
	p := &registry.Project{ID: "p", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 2}}
	base := time.Unix(1_700_000_000, 0).UTC()

	l.Record(p.ID, base)
	l.Record(p.ID, base.Add(10*time.Second))

	// Within the minute → blocked.
	d := l.Check(p, base.Add(30*time.Second))
	assert.True(t, d.Blocked)

	// 61 seconds after the first → first slides out, only one counts → allowed.
	d2 := l.Check(p, base.Add(61*time.Second))
	assert.False(t, d2.Blocked)
	assert.Equal(t, 1, d2.MinuteCount)
}

func TestCheck_PerHourBlocks(t *testing.T) {
	l := New()
	p := &registry.Project{ID: "p", RateLimit: registry.ProjectRateLimit{TasksPerHour: 5}}
	base := time.Now()

	// Five within the hour, spaced wider than a minute so per-minute (if set)
	// wouldn't matter. Here we only have per-hour set.
	for i := 0; i < 5; i++ {
		l.Record(p.ID, base.Add(time.Duration(i)*10*time.Minute))
	}
	d := l.Check(p, base.Add(50*time.Minute+5*time.Second))
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "hour")
	assert.Equal(t, 5, d.HourCount)
}

func TestCheck_IsolatedPerProject(t *testing.T) {
	l := New()
	pA := &registry.Project{ID: "a", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 1}}
	pB := &registry.Project{ID: "b", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 1}}
	now := time.Now()

	l.Record(pA.ID, now)
	assert.True(t, l.Check(pA, now).Blocked)
	// Project B has its own count.
	assert.False(t, l.Check(pB, now).Blocked)
}

func TestPruneOlderThan_KeepsOrder(t *testing.T) {
	base := time.Now()
	ts := []time.Time{
		base.Add(-2 * time.Hour),
		base.Add(-30 * time.Minute),
		base.Add(-10 * time.Minute),
		base.Add(-1 * time.Minute),
	}
	got := pruneOlderThan(ts, base.Add(-1*time.Hour))
	assert.Len(t, got, 3)
	assert.Equal(t, ts[1:], got)

	// Nothing to prune: returns original slice (pointer equality).
	got2 := pruneOlderThan(got, base.Add(-1*time.Hour))
	assert.Len(t, got2, 3)
}

func TestNilLimiterSafe(t *testing.T) {
	var l *Limiter
	p := &registry.Project{ID: "p", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 1}}
	d := l.Check(p, time.Now())
	assert.False(t, d.Blocked)
	l.Record("p", time.Now()) // no panic
}
