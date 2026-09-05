package chat

import (
	"testing"
	"time"
)

// THE SLOW-MODEL HOLE (design 2026-09-04-fallback-ladder-and-slow-model-breaker §3).
//
// Samples are stamped when a call COMPLETES and evicted by age, so three
// consecutive failures span 2×duration. A model slower than
// Window/(MinSamples-1) — 30s on the agent defaults — has its first failure
// evicted before its third is recorded, and can therefore never accumulate
// MinSamples: it can never trip, however reliably it fails.
//
// That is the regime the breaker was written for. The 2026-07-11 incident was a
// model HANGING at 247-564s; the breaker caught the fast failures, which are the
// cheap ones. zai.glm-5 at 25-174s per call fails nearly every fallback rung
// with its circuit reported CLOSED.
func TestBreaker_SlowModelTripsOnConsecutiveFailures(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	// The agent defaults.
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	// Each call takes 50s — slower than Window/(MinSamples-1) = 30s, so every
	// previous sample is evicted before the next is recorded.
	for i := 1; i <= 3; i++ {
		now = now.Add(50 * time.Second)
		state, tripped, _ := b.Record(false, false)
		if i < 3 {
			if state != CircuitClosed {
				t.Fatalf("failure %d: state %v, want closed (not enough consecutive failures yet)", i, state)
			}
			continue
		}
		if !tripped || state != CircuitOpen {
			t.Fatalf("three consecutive failures 50s apart did not trip: state=%v tripped=%t — "+
				"a model slower than the window can never be seen", state, tripped)
		}
	}
	if got := b.TripReason(); got != TripReasonConsecutive {
		t.Errorf("trip reason = %q, want %q — an operator must be able to tell this trip from a rate trip",
			got, TripReasonConsecutive)
	}
}

// A success resets the run. Two failures, a success, two failures is not three
// in a row, and a model that is working between failures is not one the breaker
// should refuse.
func TestBreaker_SuccessResetsTheConsecutiveRun(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	for _, ok := range []bool{false, false, true, false, false} {
		now = now.Add(50 * time.Second)
		if state, tripped, _ := b.Record(ok, false); tripped || state == CircuitOpen {
			t.Fatalf("tripped on a run broken by a success: sequence had no %d consecutive failures", 3)
		}
	}
}

// The counter is per-BREAKER, and a breaker is per-model: the reset holds
// across callers, because what it protects is every caller of that model.
func TestBreaker_ConsecutiveRunIsSharedAcrossCallers(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	now = now.Add(50 * time.Second)
	_, _, _ = b.Record(false, false) // caller A fails
	now = now.Add(50 * time.Second)
	_, _, _ = b.Record(true, false) // caller B succeeds — the model is alive
	now = now.Add(50 * time.Second)
	_, _, _ = b.Record(false, false) // caller A fails again
	now = now.Add(50 * time.Second)
	state, tripped, _ := b.Record(false, false)
	if tripped || state == CircuitOpen {
		t.Fatal("a success from another caller must reset the run: the breaker guards the MODEL, not one caller")
	}
}

// A half-open probe that succeeds re-closes the circuit AND clears the run, so a
// model that tripped on a transient is not one failure away from tripping again.
func TestBreaker_ProbeSuccessClearsTheConsecutiveRun(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	for i := 0; i < 3; i++ {
		now = now.Add(50 * time.Second)
		_, _, _ = b.Record(false, false)
	}
	now = now.Add(31 * time.Second)
	permitted, probe, _ := b.Allow()
	if !permitted || !probe {
		t.Fatalf("after the cooldown the next caller must probe: permitted=%t probe=%t", permitted, probe)
	}
	if state, _, _ := b.Record(true, true); state != CircuitClosed {
		t.Fatalf("a successful probe must re-close: state=%v", state)
	}
	// One failure after recovery must not immediately re-trip.
	now = now.Add(50 * time.Second)
	if state, tripped, _ := b.Record(false, false); tripped || state == CircuitOpen {
		t.Fatal("the consecutive run survived the probe's success — a recovered model is one failure from tripping")
	}
}

// The existing rate-in-window trip is untouched: fast failures inside the
// window still trip on rate, and they report so.
func TestBreaker_RateTripStillFiresAndSaysSo(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	// Interleave a success so the run never reaches 3 — the rate test is what
	// must fire here (4 failures of 5 samples = 80% ≥ 50%).
	for _, ok := range []bool{false, false, true, false, false} {
		now = now.Add(time.Second)
		_, _, _ = b.Record(ok, false)
	}
	state, _ := b.Snapshot()
	if state != "open" {
		t.Fatalf("the rate trip stopped firing: state=%q", state)
	}
	if got := b.TripReason(); got != TripReasonRate {
		t.Errorf("trip reason = %q, want %q", got, TripReasonRate)
	}
}

// A probe failure re-opens the circuit, and it is its OWN trip reason. Leaving
// the previous rule's label there counted every re-open as another instance of
// whatever opened the circuit first, which is the one thing the label exists to
// prevent (review-20260904-bef3, suggestion 3).
func TestBreaker_ProbeFailureIsItsOwnTripReason(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	b := NewBreaker(HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5,
		OpenCooldown: 30 * time.Second}, clock)

	for i := 0; i < 3; i++ {
		now = now.Add(50 * time.Second)
		_, _, _ = b.Record(false, false)
	}
	if got := b.TripReason(); got != TripReasonConsecutive {
		t.Fatalf("setup: trip reason = %q, want %q", got, TripReasonConsecutive)
	}

	now = now.Add(31 * time.Second)
	if _, probe, _ := b.Allow(); !probe {
		t.Fatal("setup: the next caller after the cooldown must be the probe")
	}
	state, tripped, _ := b.Record(false, true)
	if !tripped || state != CircuitOpen {
		t.Fatalf("a failed probe must re-open: state=%v tripped=%t", state, tripped)
	}
	if got := b.TripReason(); got != TripReasonProbeFailed {
		t.Errorf("trip reason = %q, want %q — the re-open is not another rate or consecutive trip",
			got, TripReasonProbeFailed)
	}
}
