package scheduler

import (
	"testing"
	"time"
)

// TestRecordRecoveryIdleSince_ReturnsFirstObservation pins the
// load-bearing semantics of the recovery grace period (LLD §6).
// First call stamps now and returns it. Subsequent calls return
// the FIRST observation, regardless of how much time has passed —
// the recovery sweep relies on this to compute the continuous-idle
// duration.
func TestRecordRecoveryIdleSince_ReturnsFirstObservation(t *testing.T) {
	s := &Scheduler{recoveryIdleSince: map[string]time.Time{}}
	first := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	later := first.Add(45 * time.Second)

	got1 := s.recordRecoveryIdleSince("task-1", first)
	if !got1.Equal(first) {
		t.Errorf("first observation: got %v, want %v", got1, first)
	}

	got2 := s.recordRecoveryIdleSince("task-1", later)
	if !got2.Equal(first) {
		t.Errorf("second observation should return first-seen time; got %v, want %v", got2, first)
	}
}

// TestClearRecoveryIdleSince_ResetsObservation pins the
// transient-recovery story: a task that briefly went idle
// (IsExecuting=false for one tick) followed by a true reading
// must not carry the stale idle window forward. clearRecoveryIdleSince
// is what the recovery sweep calls when IsExecuting flips back to
// true; the next idle observation must start the grace clock fresh.
func TestClearRecoveryIdleSince_ResetsObservation(t *testing.T) {
	s := &Scheduler{recoveryIdleSince: map[string]time.Time{}}
	first := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	intermediate := first.Add(45 * time.Second) // halfway through grace window
	resumed := first.Add(60 * time.Second)
	resumedAgain := first.Add(120 * time.Second)

	s.recordRecoveryIdleSince("task-1", first)
	// IsExecuting flipped to true between first and resumed —
	// the recovery sweep clears the observation.
	_ = intermediate
	s.clearRecoveryIdleSince("task-1")

	// Next idle observation must use `resumed`, not `first` —
	// otherwise a transient-true gap could be silently bridged
	// and the task gets re-queued after one continuous-false
	// tick that should have been within a fresh grace window.
	got := s.recordRecoveryIdleSince("task-1", resumed)
	if !got.Equal(resumed) {
		t.Errorf("after clear, observation must restart; got %v, want %v", got, resumed)
	}

	// And persists across subsequent observations until cleared
	// again.
	got = s.recordRecoveryIdleSince("task-1", resumedAgain)
	if !got.Equal(resumed) {
		t.Errorf("post-clear chain: got %v, want %v (the post-clear first-seen)", got, resumed)
	}
}

// TestRecoveryIdleSince_PerTaskIsolation — two tasks observed idle
// at different times must track independently. A regression here
// would cross-contaminate the grace clocks.
func TestRecoveryIdleSince_PerTaskIsolation(t *testing.T) {
	s := &Scheduler{recoveryIdleSince: map[string]time.Time{}}
	tA := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	tB := tA.Add(30 * time.Second)

	s.recordRecoveryIdleSince("task-A", tA)
	s.recordRecoveryIdleSince("task-B", tB)

	if got := s.recordRecoveryIdleSince("task-A", tB); !got.Equal(tA) {
		t.Errorf("task-A first-seen polluted by task-B: got %v, want %v", got, tA)
	}
	if got := s.recordRecoveryIdleSince("task-B", tA); !got.Equal(tB) {
		t.Errorf("task-B first-seen polluted by task-A: got %v, want %v", got, tB)
	}

	// Clearing one must not touch the other.
	s.clearRecoveryIdleSince("task-A")
	if _, present := s.recoveryIdleSince["task-B"]; !present {
		t.Error("clearing task-A wrongly removed task-B's observation")
	}
}

// TestDefaultConfig_RecoveryIdleGraceIsSet — the default config has
// to ship a non-zero grace window; without it, every recovery sweep
// would treat a single IsExecuting=false as orphan and reproduce
// the rotation bug we just fixed. The default needs to be at least a
// couple of recovery-interval ticks so transient gaps are absorbed.
func TestDefaultConfig_RecoveryIdleGraceIsSet(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RecoveryIdleGrace < 30*time.Second {
		t.Errorf("RecoveryIdleGrace = %v, want >= 30s (one recovery-interval tick)", cfg.RecoveryIdleGrace)
	}
	if cfg.RecoveryIdleGrace < 2*cfg.RecoveryInterval {
		t.Errorf("RecoveryIdleGrace = %v, want >= 2× RecoveryInterval (%v) so transient idle gaps are absorbed",
			cfg.RecoveryIdleGrace, cfg.RecoveryInterval)
	}
}
