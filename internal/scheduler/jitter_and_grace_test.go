package scheduler

import (
	"testing"
	"time"
)

// TestComputeJitteredRenewInterval_StaysWithinBand — the jitter must
// keep the renewal cadence in [base*0.75, base*1.25] so two
// renewals always fit per lease (giving a margin against transient
// renewal failures). 1000 trials catches a math bug that would
// occasionally produce an out-of-band value.
func TestComputeJitteredRenewInterval_StaysWithinBand(t *testing.T) {
	leaseSeconds := 300 // base = 150s, band = [112.5s, 187.5s]
	base := time.Duration(leaseSeconds) * time.Second / 2
	low := base * 3 / 4
	high := base * 5 / 4

	for i := 0; i < 1000; i++ {
		got := computeJitteredRenewInterval(leaseSeconds)
		if got < low || got > high {
			t.Fatalf("iteration %d: got %v, want [%v, %v]", i, got, low, high)
		}
	}
}

// TestComputeJitteredRenewInterval_ProducesVariety — the jitter must
// actually desynchronise (distinct values across 100 calls). A
// degenerate implementation returning a constant would defeat the
// purpose of jitter and let the thundering-herd renewal pattern
// re-emerge under load.
func TestComputeJitteredRenewInterval_ProducesVariety(t *testing.T) {
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 100; i++ {
		seen[computeJitteredRenewInterval(300)] = struct{}{}
	}
	if len(seen) < 50 {
		t.Errorf("only %d distinct intervals across 100 calls; jitter is degenerate", len(seen))
	}
}

// TestComputeJitteredRenewInterval_ClampsTinyConfigs — defensive: a
// LeaseDurationSeconds of 0 or negative would otherwise produce zero
// or negative renewals. Floor at 100ms — low enough that sub-second
// test configs (LeaseDurationSeconds=1) stay functional, high enough
// that pathological zero/negative configs don't hammer the DB.
func TestComputeJitteredRenewInterval_ClampsTinyConfigs(t *testing.T) {
	for _, sec := range []int{0, -1} {
		got := computeJitteredRenewInterval(sec)
		if got < 100*time.Millisecond {
			t.Errorf("leaseSeconds=%d produced %v, want >= 100ms", sec, got)
		}
	}
}

// TestDynamicRecoveryGrace_ScalesWithRunningCount — the grace
// adjusts with active-execution count so high-churn deployments
// don't see spurious recoveries from false IsExecuting=false
// observations. Pin the piecewise scaling so a future tweak can't
// silently regress the curve.
func TestDynamicRecoveryGrace_ScalesWithRunningCount(t *testing.T) {
	base := 90 * time.Second
	cases := []struct {
		running int
		want    time.Duration
	}{
		{running: 0, want: base},
		{running: 5, want: base},
		{running: 6, want: base * 3 / 2}, // 135s
		{running: 15, want: base * 3 / 2},
		{running: 16, want: base * 2},  // 180s
		{running: 100, want: base * 2}, // capped
	}
	for _, tc := range cases {
		s := &Scheduler{
			config:       &Config{RecoveryIdleGrace: base},
			runningCount: tc.running,
		}
		got := s.dynamicRecoveryGrace()
		if got != tc.want {
			t.Errorf("running=%d: grace = %v, want %v", tc.running, got, tc.want)
		}
	}
}

// TestDynamicRecoveryGrace_DefaultsZeroBase — when the config base
// is zero (unconfigured), the helper must fall back to 90s rather
// than producing a 0 grace. A zero grace would defeat the entire
// purpose of the recovery-loop check (every IsExecuting=false
// observation would immediately recover).
func TestDynamicRecoveryGrace_DefaultsZeroBase(t *testing.T) {
	s := &Scheduler{config: &Config{RecoveryIdleGrace: 0}, runningCount: 0}
	if got := s.dynamicRecoveryGrace(); got < 60*time.Second {
		t.Errorf("zero-base grace = %v, want >= 60s fallback", got)
	}
}
