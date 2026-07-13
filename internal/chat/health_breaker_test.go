package chat

import (
	"testing"
	"time"
)

// TestBreaker_ExportedAPI pins the exported Breaker surface that the
// executor-side agent-LLM breaker (internal/executor/agenthealth) reuses.
// The full closed→open→half-open→closed state machine is exercised by
// health_gate_test.go via HealthGatedProvider; here we only pin the
// exported constructor + Allow/Record/Snapshot/OpenSince + CircuitState
// labels so the rename from the unexported modelBreaker stays behaviour-
// identical.
func TestBreaker_ExportedAPI(t *testing.T) {
	cfg := HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second}
	b := NewBreaker(cfg, time.Now)

	// Closed circuit: calls are permitted, not probes, state label "closed".
	for i := 0; i < 3; i++ {
		permitted, probe, state := b.Allow()
		if !permitted || probe || state != CircuitClosed || state.Label() != "closed" {
			t.Fatalf("attempt %d: expected closed permit, got permitted=%v probe=%v state=%q", i, permitted, probe, state.Label())
		}
		// Infra failure. After MinSamples=3 sustained failures (100% >= 0.5)
		// the circuit trips on the 3rd record.
		newState, tripped, changed := b.Record(false, false)
		_ = newState
		if i == 2 && !tripped {
			t.Fatalf("expected trip on 3rd failure, got tripped=%v changed=%v", tripped, changed)
		}
	}

	// Now OPEN: calls fast-reject, no probe permit consumed.
	permitted, probe, state := b.Allow()
	if permitted || probe || state != CircuitOpen || state.Label() != "open" {
		t.Fatalf("expected open reject, got permitted=%v probe=%v state=%q", permitted, probe, state.Label())
	}
	if got := b.OpenSince(); got.IsZero() {
		t.Fatal("OpenSince should be set when open")
	}
	st, openedAt := b.Snapshot()
	if st != "open" || openedAt.IsZero() {
		t.Fatalf("snapshot = (%q, %v), want (open, non-zero)", st, openedAt)
	}

	// A success record on a non-probe closed-state call does not trip;
	// exercised via a fresh breaker so the open one above isn't disturbed.
	good := NewBreaker(cfg, time.Now)
	good.Record(true, false) // success
	if ps, _, _ := good.Allow(); !ps {
		t.Fatal("success should not trip a closed circuit")
	}
}
