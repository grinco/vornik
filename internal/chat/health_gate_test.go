package chat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeInner is a call-counting, scriptable Provider for breaker tests.
type fakeInner struct {
	model string
	calls atomic.Int64
	// errFn returns the error for call N (1-based); nil error = success.
	errFn func(n int64) error
}

func (f *fakeInner) do() error {
	n := f.calls.Add(1)
	if f.errFn == nil {
		return nil
	}
	return f.errFn(n)
}
func (f *fakeInner) Complete(context.Context, []Message) (*ChatResponse, error) {
	return &ChatResponse{}, f.do()
}
func (f *fakeInner) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return &ChatResponse{}, f.do()
}
func (f *fakeInner) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{}, f.do()
}
func (f *fakeInner) Model() string       { return f.model }
func (f *fakeInner) SetMetrics(*Metrics) {}
func (f *fakeInner) WithModel(m string) Provider {
	// share the same call counter across clones (same underlying upstream)
	return &fakeInner{model: m, errFn: f.errFn}
}

func testCfg() HealthConfig {
	return HealthConfig{Window: time.Minute, MinSamples: 5, FailureRate: 0.5, OpenCooldown: 30 * time.Second}
}

// clock is a mutable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() } //nolint:unparam // test helper: duration kept explicit for readability

func newGate(inner Provider, cfg HealthConfig, now func() time.Time) *HealthGatedProvider {
	return newHealthGatedProvider(inner, "test-route", &sync.Map{}, cfg, now)
}

func alwaysInfra(int64) error { return &GatewayError{Status: 503} }

func TestBreaker_TripsAtThreshold(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: alwaysInfra}
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()

	// 4 failures: below MinSamples=5 → still closed, still calls upstream.
	for i := 0; i < 4; i++ {
		if _, err := g.Complete(ctx, nil); !IsUpstreamInfraError(err) {
			t.Fatalf("call %d: want infra error passthrough, got %v", i, err)
		}
	}
	if IsModelUnhealthy(mustErr(g.Complete(ctx, nil))) {
		// this is the 5th failure — trips AFTER recording, so this call still
		// went upstream; the NEXT call rejects.
		t.Fatal("5th failing call should still reach upstream (trips on record)")
	}
	before := inner.calls.Load()
	// Now OPEN: next call fast-rejects without calling upstream.
	_, err := g.Complete(ctx, nil)
	if !IsModelUnhealthy(err) {
		t.Fatalf("want ModelUnhealthyError once open, got %v", err)
	}
	if inner.calls.Load() != before {
		t.Fatal("open circuit must NOT call upstream")
	}
}

// Off-by-one: MinSamples-1 failures at 100% does not trip.
func TestBreaker_MinSamplesBoundary(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: alwaysInfra}
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()
	for i := 0; i < 4; i++ { // MinSamples-1 = 4
		_, _ = g.Complete(ctx, nil)
	}
	// Still closed: the next call reaches upstream (not a fast reject).
	c := inner.calls.Load()
	_, _ = g.Complete(ctx, nil)
	if inner.calls.Load() != c+1 {
		t.Fatal("below MinSamples must stay closed and call upstream")
	}
}

// Below FailureRate does not trip even past MinSamples.
func TestBreaker_BelowFailureRate(t *testing.T) {
	clk := &clock{t: time.Now()}
	n := int64(0)
	inner := &fakeInner{model: "m", errFn: func(int64) error {
		n++
		if n%5 == 0 { // 20% failure, below 0.5
			return &GatewayError{Status: 503}
		}
		return nil
	}}
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if _, err := g.Complete(ctx, nil); IsModelUnhealthy(err) {
			t.Fatalf("must not trip below FailureRate (call %d)", i)
		}
	}
}

func TestBreaker_HalfOpenSinglePermitAndRecover(t *testing.T) {
	clk := &clock{t: time.Now()}
	// The probe (call 6) BLOCKS until released, so the 7 concurrent callers all
	// arrive while the circuit is HALF_OPEN with the permit taken → they must
	// reject. A non-blocking probe would close the circuit before they arrive,
	// which is also correct but wouldn't exercise the single-permit gate.
	probeStarted := make(chan struct{})
	release := make(chan struct{})
	inner := &fakeInner{model: "m", errFn: func(n int64) error {
		if n <= 5 {
			return &GatewayError{Status: 503}
		}
		if n == 6 { // only the probe blocks
			close(probeStarted)
			<-release
		}
		return nil
	}}
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = g.Complete(ctx, nil)
	}
	if !IsModelUnhealthy(mustErr(g.Complete(ctx, nil))) {
		t.Fatal("expected open after 5 failures")
	}
	clk.add(31 * time.Second)
	callsBefore := inner.calls.Load()

	// Fire the probe (will block inside the inner call).
	var probeErr atomic.Value
	go func() { _, err := g.Complete(ctx, nil); probeErr.Store(errBox{err}) }()
	<-probeStarted // probe is now in-flight, circuit HALF_OPEN, permit held

	// All other callers arriving now must fast-reject.
	var rejects atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Complete(ctx, nil); IsModelUnhealthy(err) {
				rejects.Add(1)
			}
		}()
	}
	wg.Wait()
	if rejects.Load() != 7 {
		t.Fatalf("all 7 non-probe callers must reject while the probe holds the permit, got %d", rejects.Load())
	}
	if inner.calls.Load() != callsBefore+1 {
		t.Fatalf("only the probe may reach upstream, got %d extra calls", inner.calls.Load()-callsBefore)
	}
	// Release the probe → success → CLOSED.
	close(release)
	// Give the probe goroutine a moment (real time, tiny) to record.
	for i := 0; i < 100; i++ {
		if probeErr.Load() != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := g.Complete(ctx, nil); IsModelUnhealthy(err) {
		t.Fatal("circuit should be closed after a successful probe")
	}
}

type errBox struct{ err error }

func TestBreaker_ProbeFailureReopens(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: alwaysInfra} // always fails
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		_, _ = g.Complete(ctx, nil)
	}
	clk.add(31 * time.Second)
	// probe fails → reopen; next call before a fresh cooldown rejects.
	_, _ = g.Complete(ctx, nil) // the probe
	if !IsModelUnhealthy(mustErr(g.Complete(ctx, nil))) {
		t.Fatal("a failed probe must re-open the circuit")
	}
}

// A half-open probe that returns a SHAPE error (non-infra) closes the circuit
// — the model is reachable (§12.3).
func TestBreaker_ProbeShapeErrorCountsAsSuccess(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: func(n int64) error {
		if n <= 5 {
			return &GatewayError{Status: 503}
		}
		return errors.New("schema violation: role missing required keys")
	}}
	g := newGate(inner, testCfg(), clk.now)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		_, _ = g.Complete(ctx, nil)
	}
	clk.add(31 * time.Second)
	// probe returns a shape error → treated as success → CLOSED.
	if _, err := g.Complete(ctx, nil); IsModelUnhealthy(err) {
		t.Fatal("probe should have been permitted")
	}
	if _, err := g.Complete(ctx, nil); IsModelUnhealthy(err) {
		t.Fatal("shape-error probe must close the circuit (model is up)")
	}
}

// context.Canceled does not increment failures; DeadlineExceeded does.
func TestBreaker_CancelVsDeadline(t *testing.T) {
	clk := &clock{t: time.Now()}
	innerCancel := &fakeInner{model: "m", errFn: func(int64) error { return context.Canceled }}
	g := newGate(innerCancel, testCfg(), clk.now)
	for i := 0; i < 20; i++ {
		if _, err := g.Complete(context.Background(), nil); IsModelUnhealthy(err) {
			t.Fatal("cancellations must never trip the breaker")
		}
	}

	innerDeadline := &fakeInner{model: "m", errFn: func(int64) error { return context.DeadlineExceeded }}
	g2 := newGate(innerDeadline, testCfg(), clk.now)
	for i := 0; i < 5; i++ {
		_, _ = g2.Complete(context.Background(), nil)
	}
	if !IsModelUnhealthy(mustErr(g2.Complete(context.Background(), nil))) {
		t.Fatal("deadlines must trip the breaker")
	}
}

// WithModel clone shares the registry; per-(route,model) isolation holds.
func TestBreaker_WithModelSharesRegistryAndIsolatesModels(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "primary", errFn: alwaysInfra}
	g := newGate(inner, testCfg(), clk.now)
	// Trip "primary".
	for i := 0; i < 6; i++ {
		_, _ = g.Complete(context.Background(), nil)
	}
	// A clone pinned to the SAME model sees the open circuit (shared registry).
	same := g.WithModel("primary")
	if !IsModelUnhealthy(mustErr(same.Complete(context.Background(), nil))) {
		t.Fatal("clone on the same model must see the shared open circuit")
	}
	// A clone pinned to a DIFFERENT model is independent (still closed).
	other := g.WithModel("secondary")
	if _, err := other.Complete(context.Background(), nil); IsModelUnhealthy(err) {
		t.Fatal("a different model on the same route must be isolated")
	}
}

// Concurrent first-calls for a new key create exactly one breaker.
func TestBreaker_LoadOrStoreNoDuplicate(t *testing.T) {
	clk := &clock{t: time.Now()}
	reg := &sync.Map{}
	inner := &fakeInner{model: "m", errFn: alwaysInfra}
	g := newHealthGatedProvider(inner, "r", reg, testCfg(), clk.now)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = g.Complete(context.Background(), nil) }()
	}
	wg.Wait()
	count := 0
	reg.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("want exactly 1 breaker in the registry, got %d", count)
	}
}

func mustErr(_ *ChatResponse, err error) error { return err }

// The onChange hook fires on every state transition (edge-triggered), for the
// operator-alert / observability seam (§7).
func TestBreaker_StateChangeHookFires(t *testing.T) {
	clk := &clock{t: time.Now()}
	// fail 1-5, then succeed (so the half-open probe recovers).
	inner := &fakeInner{model: "m", errFn: func(n int64) error {
		if n <= 5 {
			return &GatewayError{Status: 503}
		}
		return nil
	}}
	g := newGate(inner, testCfg(), clk.now)
	var mu sync.Mutex
	var transitions []string
	g.WithStateChangeHook(func(route, model, state string) {
		mu.Lock()
		transitions = append(transitions, route+"/"+model+"="+state)
		mu.Unlock()
	})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = g.Complete(ctx, nil) // 5th failure trips → one "open"
	}
	clk.add(31 * time.Second)
	_, _ = g.Complete(ctx, nil) // probe succeeds → "closed"
	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 {
		t.Fatalf("want 2 transitions (open, closed), got %v", transitions)
	}
	if transitions[0] != "test-route/m=open" || transitions[1] != "test-route/m=closed" {
		t.Fatalf("unexpected transitions: %v", transitions)
	}
}

// TestModelHealthSnapshot_ReflectsCircuitState — the doctor's live-circuit
// read path. After a model's breaker trips OPEN, ModelHealthSnapshot must
// report it; a second, healthy model on the same shared registry stays
// closed. Backlog item 5 (doctor live-circuit line).
func TestModelHealthSnapshot_ReflectsCircuitState(t *testing.T) {
	clk := &clock{t: time.Now()}
	reg := &sync.Map{}
	badInner := &fakeInner{model: "bad", errFn: alwaysInfra}
	bad := newHealthGatedProvider(badInner, "route-a", reg, testCfg(), clk.now)
	goodInner := &fakeInner{model: "good", errFn: nil}
	good := newHealthGatedProvider(goodInner, "route-b", reg, testCfg(), clk.now)
	ctx := context.Background()

	// Trip route-a/bad OPEN (5 infra failures record, then it's open).
	for i := 0; i < 5; i++ {
		_, _ = bad.Complete(ctx, nil)
	}
	// One healthy call on route-b/good keeps its circuit closed.
	_, _ = good.Complete(ctx, nil)

	// Both providers share the registry, so either yields the full set.
	snaps := bad.ModelHealthSnapshot()
	byKey := map[string]ModelHealthSnapshot{}
	for _, s := range snaps {
		byKey[s.Route+"/"+s.Model] = s
	}
	if got := byKey["route-a/bad"]; got.State != "open" {
		t.Errorf("route-a/bad state = %q, want open", got.State)
	}
	if got, ok := byKey["route-b/good"]; !ok || got.State != "closed" {
		t.Errorf("route-b/good = %+v (ok=%v), want closed", got, ok)
	}
	if byKey["route-a/bad"].OpenSince.IsZero() {
		t.Error("open circuit must carry a non-zero OpenSince")
	}
}

// TestRouterModelHealthSnapshot_DedupsSharedRegistry — the Router walk
// de-dups (route, model) across sub-providers that share one registry.
func TestRouterModelHealthSnapshot_DedupsSharedRegistry(t *testing.T) {
	clk := &clock{t: time.Now()}
	reg := &sync.Map{}
	fb := newHealthGatedProvider(&fakeInner{model: "fb"}, "", reg, testCfg(), clk.now)
	ra := newHealthGatedProvider(&fakeInner{model: "ra"}, "route-a", reg, testCfg(), clk.now)
	_, _ = fb.Complete(context.Background(), nil)
	_, _ = ra.Complete(context.Background(), nil)

	r := &Router{fallback: fb, routes: []Route{{Name: "route-a", Provider: ra}}}
	snaps := r.ModelHealthSnapshot()
	keys := map[string]int{}
	for _, s := range snaps {
		keys[s.Route+"/"+s.Model]++
	}
	if keys["/fb"] != 1 || keys["route-a/ra"] != 1 {
		t.Errorf("expected one entry per circuit, got %+v", keys)
	}
}
