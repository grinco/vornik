package agenthealth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// fakeClock is a controllable now() for deterministic trip/probe timing.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// agentCfg is the design's agent default (MinSamples=3, FailureRate=0.5,
// OpenCooldown=30s, Window=1m) — see LLD §5/§6.
func agentCfg() chat.HealthConfig {
	return chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second}
}

func newReg(cfg chat.HealthConfig, enabled bool) *Registry {
	return NewRegistry(Config{Health: cfg, Enabled: enabled, Now: (&fakeClock{t: time.Unix(1_700_000_000, 0)}).now})
}

// TestRecord_ClassificationMatrix pins the §4 Record classification table.
func TestRecord_ClassificationMatrix(t *testing.T) {
	r := newReg(agentCfg(), true)
	// nil -> success (no trip).
	r.Record("m", false, nil)
	if _, _, reject := r.Gate("m"); reject != nil {
		t.Fatalf("nil should be success (no trip); gate rejected: %v", reject)
	}
	// infra (PROVIDER_ERROR) -> failure. 3 sustained failures trip (MinSamples=3).
	for i := 0; i < 3; i++ {
		r.Record("m", false, fmt.Errorf("agent reported FAILED status: PROVIDER_ERROR: upstream provider returned an error"))
	}
	permitted, _, reject := r.Gate("m")
	if permitted {
		t.Fatalf("3 sustained infra failures should trip OPEN; gate permitted")
	}
	var mue *chat.ModelUnhealthyError
	if !errors.As(reject, &mue) {
		t.Fatalf("reject should be *chat.ModelUnhealthyError, got %T: %v", reject, reject)
	}
	if mue.Route != "agent" || mue.Model != "m" || mue.State != "open" {
		t.Fatalf("reject has wrong shape: %+v", mue)
	}
	if !chat.IsModelUnhealthy(reject) {
		t.Fatalf("chat.IsModelUnhealthy must match the reject")
	}
}

// TestRecord_AbstainOnModelUnhealthy — a chat-breaker MODEL_UNHEALTHY reject
// (topology 2) is abstained: neither success nor failure (LLD §4 abstain row).
func TestRecord_AbstainOnModelUnhealthy(t *testing.T) {
	r := newReg(agentCfg(), true)
	// 5 MODEL_UNHEALTHY errors must NOT trip (abstain — no votes recorded).
	for i := 0; i < 5; i++ {
		r.Record("m", false, &chat.ModelUnhealthyError{Route: "fallback", Model: "m", State: "open"})
	}
	if _, _, reject := r.Gate("m"); reject != nil {
		t.Fatalf("MODEL_UNHEALTHY errors must be abstain (no trip); got reject: %v", reject)
	}
	// Also the agent-emitted marker string form.
	for i := 0; i < 5; i++ {
		r.Record("m2", false, errors.New("LLM call failed: MODEL_UNHEALTHY: model \"m2\" circuit open"))
	}
	if _, _, reject := r.Gate("m2"); reject != nil {
		t.Fatalf("agent-emitted MODEL_UNHEALTHY marker must be abstain; got reject: %v", reject)
	}
}

// TestRecord_ShapeErrorIsSuccess — a shape/content error (model reachable,
// bad output) counts as success, not failure (LLD §4).
func TestRecord_ShapeErrorIsSuccess(t *testing.T) {
	r := newReg(agentCfg(), true)
	for i := 0; i < 5; i++ {
		r.Record("m", false, errors.New("schema violation: missing required field \"approved\""))
	}
	if _, _, reject := r.Gate("m"); reject != nil {
		t.Fatalf("shape errors are success (model reachable); should not trip; got reject: %v", reject)
	}
}

// TestRecord_CancelAbstainsDeadlineFails — context.Canceled is abstain (caller
// gave up); DeadlineExceeded is a failure (LLD §12 I10).
func TestRecord_CancelAbstainsDeadlineFails(t *testing.T) {
	r := newReg(agentCfg(), true)
	for i := 0; i < 5; i++ {
		r.Record("cancel", false, context.Canceled)
	}
	if _, _, reject := r.Gate("cancel"); reject != nil {
		t.Fatalf("context.Canceled must be abstain (no trip); got reject: %v", reject)
	}
	for i := 0; i < 3; i++ {
		r.Record("deadline", false, context.DeadlineExceeded)
	}
	if _, _, reject := r.Gate("deadline"); reject == nil {
		t.Fatalf("context.DeadlineExceeded must be a failure (trip after MinSamples); gate permitted")
	}
}

// TestGate_HalfOpenProbe — after OpenCooldown the first Gate call gets the
// single probe permit; concurrent callers are rejected until the probe resolves.
func TestGate_HalfOpenProbe(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := NewRegistry(Config{Health: agentCfg(), Enabled: true, Now: clock.now})
	for i := 0; i < 3; i++ {
		r.Record("m", false, errors.New("PROVIDER_ERROR"))
	}
	if _, _, reject := r.Gate("m"); reject == nil {
		t.Fatalf("should be OPEN")
	}
	clock.advance(31 * time.Second) // past OpenCooldown -> half-open
	permitted, probe, reject := r.Gate("m")
	if !permitted || !probe || reject != nil {
		t.Fatalf("first post-cooldown call should be the probe permit, got permitted=%v probe=%v reject=%v", permitted, probe, reject)
	}
	// A concurrent caller while the probe is in flight is rejected (half-open permit taken).
	if _, _, r2 := r.Gate("m"); r2 == nil {
		t.Fatalf("concurrent caller while probe in flight must be rejected")
	}
	// Probe succeeds -> circuit closes.
	r.Record("m", true, nil)
	permitted, probe, reject = r.Gate("m")
	if !permitted || probe || reject != nil {
		t.Fatalf("after successful probe the circuit should be CLOSED, got permitted=%v probe=%v reject=%v", permitted, probe, reject)
	}
}

// TestRegistry_Shared — two Record callers for the same model vote into one breaker.
func TestRegistry_Shared(t *testing.T) {
	r := newReg(agentCfg(), true)
	// Caller A records 2 failures, caller B records 1 failure -> 3 total -> trip.
	r.Record("m", false, errors.New("PROVIDER_ERROR"))
	r.Record("m", false, errors.New("PROVIDER_ERROR"))
	r.Record("m", false, errors.New("PROVIDER_ERROR"))
	if _, _, reject := r.Gate("m"); reject == nil {
		t.Fatalf("3 failures across callers should trip the shared breaker")
	}
}

// TestDisabled_Passthrough — enabled:false Gate/Record are no-ops.
func TestDisabled_Passthrough(t *testing.T) {
	r := newReg(agentCfg(), false)
	for i := 0; i < 10; i++ {
		r.Record("m", false, errors.New("PROVIDER_ERROR"))
	}
	permitted, probe, reject := r.Gate("m")
	if !permitted || probe || reject != nil {
		t.Fatalf("disabled registry: Gate must be passthrough (true,false,nil), got permitted=%v probe=%v reject=%v", permitted, probe, reject)
	}
}

// TestNilRegistry_NoPanic — a nil registry (not wired) must not panic (LLD §12 B3).
func TestNilRegistry_NoPanic(t *testing.T) {
	var r *Registry
	permitted, probe, reject := r.Gate("m")
	if !permitted || probe || reject != nil {
		t.Fatalf("nil registry Gate must be passthrough, got permitted=%v probe=%v reject=%v", permitted, probe, reject)
	}
	r.Record("m", false, errors.New("PROVIDER_ERROR")) // must not panic
}

// TestEmptyModel_Passthrough — an empty model string (unresolved) is a no-op.
func TestEmptyModel_Passthrough(t *testing.T) {
	r := newReg(agentCfg(), true)
	r.Record("", false, errors.New("PROVIDER_ERROR"))
	if _, _, reject := r.Gate(""); reject != nil {
		t.Fatalf("empty model must be passthrough, got reject: %v", reject)
	}
}

// TestNilMetrics_NoPanic — a nil metrics sink must not panic; the breaker still gates.
func TestNilMetrics_NoPanic(t *testing.T) {
	r := newReg(agentCfg(), true)
	r.SetMetrics(nil) // explicit nil
	for i := 0; i < 3; i++ {
		r.Record("m", false, errors.New("PROVIDER_ERROR")) // exercises setGauge/incTrips nil path
	}
	if _, _, reject := r.Gate("m"); reject == nil {
		t.Fatalf("breaker must trip even with nil metrics")
	}
}

// recordingSink is a test MetricsSink that records calls.
type recordingSink struct {
	mu     sync.Mutex
	states map[string]int
	trips  map[string]int
}

func (s *recordingSink) SetStateGauge(model string, state chat.CircuitState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[model] = int(state)
}
func (s *recordingSink) IncTrips(model, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trips[model]++
}

// TestMetricsSink_Wired — the sink receives state gauge + trips on transitions.
func TestMetricsSink_Wired(t *testing.T) {
	r := newReg(agentCfg(), true)
	sink := &recordingSink{states: map[string]int{}, trips: map[string]int{}}
	r.SetMetrics(sink)
	for i := 0; i < 3; i++ {
		r.Record("m", false, errors.New("PROVIDER_ERROR"))
	}
	if sink.trips["m"] != 1 {
		t.Fatalf("expected 1 trip, got %d", sink.trips["m"])
	}
	if sink.states["m"] != int(chat.CircuitOpen) {
		t.Fatalf("expected state=open(2), got %d", sink.states["m"])
	}
}

// TestConcurrentRecord_NoRace — N goroutines calling Record for the same model
// converge on a consistent breaker state (run with -race). LLD §12 M11.
func TestConcurrentRecord_NoRace(t *testing.T) {
	r := newReg(agentCfg(), true)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Record("m", false, errors.New("PROVIDER_ERROR"))
		}()
	}
	wg.Wait()
	// 50 failures >> MinSamples=3; must be OPEN.
	if _, _, reject := r.Gate("m"); reject == nil {
		t.Fatalf("50 concurrent failures should leave the breaker OPEN")
	}
}

// TestGate_RejectsFastAcrossSteps — once tripped, subsequent Gate calls
// fast-reject (no upstream) — the cross-step protection (LLD §5).
func TestGate_RejectsFastAcrossSteps(t *testing.T) {
	r := newReg(agentCfg(), true)
	for i := 0; i < 3; i++ {
		r.Record("m", false, errors.New("PROVIDER_ERROR"))
	}
	var calls int32
	for i := 0; i < 10; i++ {
		if _, _, reject := r.Gate("m"); reject != nil {
			atomic.AddInt32(&calls, 1)
		}
	}
	if calls != 10 {
		t.Fatalf("all 10 subsequent gate calls must fast-reject; got %d", calls)
	}
}

// TestModelHealthSnapshot pins the doctor-facing accessor (unify-observability
// LLD 2026-07-18): every agent breaker is reported with Route=="agent" and its
// live state, so the doctor can surface the agent breaker alongside the chat one.
func TestModelHealthSnapshot(t *testing.T) {
	r := newReg(agentCfg(), true)
	// Drive "m" OPEN via 3 sustained infra failures (MinSamples=3).
	for i := 0; i < 3; i++ {
		r.Record("m", false, fmt.Errorf("PROVIDER_ERROR: upstream provider returned an error"))
	}
	// A healthy breaker (a success record creates it, closed).
	r.Record("healthy", false, nil)

	snaps := r.ModelHealthSnapshot()
	byModel := map[string]chat.ModelHealthSnapshot{}
	for _, s := range snaps {
		if s.Route != "agent" {
			t.Errorf("snapshot %+v: Route=%q, want \"agent\"", s, s.Route)
		}
		byModel[s.Model] = s
	}
	if byModel["m"].State != "open" {
		t.Errorf("model m: State=%q, want open (snaps=%v)", byModel["m"].State, snaps)
	}
	if s, ok := byModel["healthy"]; !ok || s.State != "closed" {
		t.Errorf("healthy breaker: got %+v (ok=%v), want closed", s, ok)
	}
}

// TestModelHealthSnapshot_NilRegistry pins the nil-safety invariant (review M1).
func TestModelHealthSnapshot_NilRegistry(t *testing.T) {
	var r *Registry
	if got := r.ModelHealthSnapshot(); got != nil {
		t.Errorf("nil registry ModelHealthSnapshot() = %v, want nil", got)
	}
}
