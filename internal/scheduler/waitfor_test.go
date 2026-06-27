package scheduler

import (
	"testing"
	"time"
)

// waitFor polls cond until it returns true or timeout elapses, and
// returns whether it succeeded. Use it instead of a fixed time.Sleep
// before asserting on async work (a counter that a goroutine bumps, a
// status flip, a lease renewal). A fixed sleep races that work: under
// `-race` + coverage instrumentation in the full-suite CI run, the
// worker goroutine can be starved past any hard-coded wait, leaving the
// assertion looking at stale state — a wall-clock flake, not a bug
// (see the de-flaked lease-renewal + lease-expiry-recovery tests).
//
// Pick a generous timeout (seconds): it only bounds the failure case,
// since cond is re-checked every pollStep and the loop returns as soon
// as it holds, so a fast machine still finishes in milliseconds.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	const pollStep = 10 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pollStep)
	}
}
