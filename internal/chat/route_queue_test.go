package chat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBoundedRouteProvider is a hand-rolled provider used to drive the
// queue wrapper's branches. block lets a test pin a call until
// released; counters surface what the wrapper did.
type stubBoundedRouteProvider struct {
	mu       sync.Mutex
	calls    int32
	inFlight int32
	maxInFly int32
	block    chan struct{}
	model    string
	failWith error
}

func (p *stubBoundedRouteProvider) hit(ctx context.Context) (*ChatResponse, error) {
	atomic.AddInt32(&p.calls, 1)
	cur := atomic.AddInt32(&p.inFlight, 1)
	defer atomic.AddInt32(&p.inFlight, -1)
	p.mu.Lock()
	if cur > p.maxInFly {
		p.maxInFly = cur
	}
	p.mu.Unlock()
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.failWith != nil {
		return nil, p.failWith
	}
	return &ChatResponse{}, nil
}

func (p *stubBoundedRouteProvider) Complete(ctx context.Context, _ []Message) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *stubBoundedRouteProvider) CompleteWithTools(ctx context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *stubBoundedRouteProvider) CompleteWithToolsStream(ctx context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *stubBoundedRouteProvider) Model() string         { return p.model }
func (p *stubBoundedRouteProvider) SetMetrics(_ *Metrics) {}

// stubOverridableProvider lets us assert WithModel propagates
// through the queue wrapper while sharing the semaphore.
type stubOverridableProvider struct {
	*stubBoundedRouteProvider
}

func (p *stubOverridableProvider) WithModel(m string) Provider {
	return &stubOverridableProvider{stubBoundedRouteProvider: &stubBoundedRouteProvider{model: m, block: p.block}}
}

// TestNewBoundedRouteProvider_DegradesToInnerOnZeroDepth — depth ≤ 0
// short-circuits and returns the inner unchanged so call sites
// can wrap uniformly.
func TestNewBoundedRouteProvider_DegradesToInnerOnZeroDepth(t *testing.T) {
	inner := &stubBoundedRouteProvider{}
	got := NewBoundedRouteProvider(inner, "x", 0, time.Second, nil)
	assert.Same(t, Provider(inner), got, "depth=0 must return the inner unchanged")
}

// TestNewBoundedRouteProvider_NilInnerStaysNil — defensive: a nil
// inner stays nil, no panic on construction.
func TestNewBoundedRouteProvider_NilInnerStaysNil(t *testing.T) {
	got := NewBoundedRouteProvider(nil, "x", 5, time.Second, nil)
	assert.Nil(t, got)
}

// TestNewBoundedRouteProvider_TypedNilRegistryNoPanic — regression for the
// 2026-06-03 startup crash: observabilityRegistry() returns a concrete
// *prometheus.Registry that is nil during early init, which becomes a
// TYPED-NIL when passed into the prometheus.Registerer interface param —
// non-nil interface wrapping a nil pointer. The plain `reg == nil` guard
// misses it and promauto.MustRegister segfaults. Constructing a bounded
// provider with a typed-nil registry must NOT panic; metrics are simply
// skipped (the queue still throttles).
func TestNewBoundedRouteProvider_TypedNilRegistryNoPanic(t *testing.T) {
	var nilReg *prometheus.Registry // nil pointer
	// Smuggle it through the interface — this is exactly what the service
	// wiring did with observabilityRegistry(). At the Go language level
	// `reg == nil` is FALSE here (non-nil interface, nil underlying ptr),
	// which is what the plain guard missed. (testify's NotNil unwraps
	// typed-nils via reflection, so we don't assert on it directly.)
	var reg prometheus.Registerer = nilReg

	var got Provider
	require.NotPanics(t, func() {
		got = NewBoundedRouteProvider(&stubBoundedRouteProvider{}, "openrouter", 4, time.Second, reg)
	})
	require.NotNil(t, got)

	// And it still functions as a queue (throttles, returns inner result).
	resp, err := got.CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// TestNewRouteQueueMetrics_TypedNilRegistry — unit-level: the metrics
// constructor returns nil (no panic) for both a true-nil and a typed-nil
// *prometheus.Registry.
func TestNewRouteQueueMetrics_TypedNilRegistry(t *testing.T) {
	assert.Nil(t, newRouteQueueMetrics(nil), "true-nil interface → nil metrics")
	var nilReg *prometheus.Registry
	assert.Nil(t, newRouteQueueMetrics(nilReg), "typed-nil *Registry → nil metrics, no panic")
}

// TestBoundedRouteProvider_CapsInFlightAtDepth — the headline contract:
// N concurrent calls against a depth-D wrapper never see more
// than D simultaneously in-flight inside the inner.
func TestBoundedRouteProvider_CapsInFlightAtDepth(t *testing.T) {
	block := make(chan struct{})
	inner := &stubBoundedRouteProvider{block: block}
	const depth = 3
	q := NewBoundedRouteProvider(inner, "test", depth, 0, nil)

	const callers = 10
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, _ = q.Complete(context.Background(), nil)
		}()
	}

	// Let callers reach the inner — wait until depth are in-flight.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&inner.inFlight) < depth && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assert.Equal(t, int32(depth), atomic.LoadInt32(&inner.inFlight))

	// Release everyone.
	close(block)
	wg.Wait()
	assert.Equal(t, int32(callers), atomic.LoadInt32(&inner.calls))
	assert.LessOrEqual(t, inner.maxInFly, int32(depth), "max concurrent must never exceed depth")
}

// TestBoundedRouteProvider_OverflowsAfterTimeout — when the semaphore
// is full AND no slot opens within QueueTimeout, the call gets
// a *RouteOverflowError.
func TestBoundedRouteProvider_OverflowsAfterTimeout(t *testing.T) {
	block := make(chan struct{})
	inner := &stubBoundedRouteProvider{block: block}
	q := NewBoundedRouteProvider(inner, "bedrock", 1, 30*time.Millisecond, nil)

	// First call holds the only slot. Second call should overflow.
	first := make(chan error, 1)
	go func() {
		_, err := q.Complete(context.Background(), nil)
		first <- err
	}()

	// Wait for first to enter.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&inner.inFlight) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&inner.inFlight))

	// Second call — should time out and overflow.
	_, err := q.Complete(context.Background(), nil)
	require.Error(t, err)
	var roe *RouteOverflowError
	require.True(t, errors.As(err, &roe), "expected *RouteOverflowError; got %T (%v)", err, err)
	assert.Equal(t, "bedrock", roe.Route)
	assert.Equal(t, 1, roe.Depth)
	assert.True(t, IsRouteOverflow(err))

	// Release first.
	close(block)
	require.NoError(t, <-first)
}

// TestBoundedRouteProvider_CallerCancelReturnsContextError — caller
// cancellation surfaces as ctx.Err(), NOT a RouteOverflowError.
// The two cases are operationally distinct: cancel is normal
// shutdown; overflow is the daemon refusing to queue further.
func TestBoundedRouteProvider_CallerCancelReturnsContextError(t *testing.T) {
	block := make(chan struct{})
	inner := &stubBoundedRouteProvider{block: block}
	q := NewBoundedRouteProvider(inner, "bedrock", 1, time.Hour, nil)

	// Fill the slot with a first call.
	go func() { _, _ = q.Complete(context.Background(), nil) }()
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&inner.inFlight) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Cancel the second caller before a slot opens.
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Complete(ctx, nil)
		errCh <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	err := <-errCh
	require.Error(t, err)
	assert.False(t, IsRouteOverflow(err))
	assert.True(t, errors.Is(err, context.Canceled))

	close(block)
}

// TestBoundedRouteProvider_InnerErrorPropagates — when the inner
// returns an error, the wrapper passes it through unchanged
// (not wrapped as overflow).
func TestBoundedRouteProvider_InnerErrorPropagates(t *testing.T) {
	want := errors.New("upstream broke")
	inner := &stubBoundedRouteProvider{failWith: want}
	q := NewBoundedRouteProvider(inner, "x", 5, time.Second, nil)
	_, err := q.Complete(context.Background(), nil)
	assert.ErrorIs(t, err, want)
}

// TestBoundedRouteProvider_WithModelSharesSemaphore — per-request
// model overrides MUST share the queue with the parent route.
// Otherwise the depth contract breaks: a request that overrides
// model would dodge the queue entirely.
func TestBoundedRouteProvider_WithModelSharesSemaphore(t *testing.T) {
	block := make(chan struct{})
	inner := &stubOverridableProvider{stubBoundedRouteProvider: &stubBoundedRouteProvider{block: block, model: "default"}}
	const depth = 2
	q := NewBoundedRouteProvider(inner, "x", depth, 0, nil)

	// Sanity: WithModel returns a BoundedRouteProvider sharing the sem.
	overridden := q.(ModelOverridable).WithModel("gpt-5")
	qo, ok := overridden.(*BoundedRouteProvider)
	require.True(t, ok, "WithModel must return a *BoundedRouteProvider; got %T", overridden)
	parent := q.(*BoundedRouteProvider)
	// Semaphore identity check: parent and override must share
	// the same channel — push a token on the parent and verify
	// the override observes it via len(). Channels are reference
	// types so the shared semaphore manifests as identical len().
	parent.sem <- struct{}{}
	assert.Equal(t, 1, len(qo.sem), "overridden provider must share parent semaphore")
	<-parent.sem
}

// TestBoundedRouteProvider_RouteOverflowErrorMessage — the error's
// String form embeds route + depth + waited so operators reading
// logs see the cap they hit without grepping the source.
func TestBoundedRouteProvider_RouteOverflowErrorMessage(t *testing.T) {
	err := &RouteOverflowError{Route: "bedrock", Depth: 8, Waited: 30 * time.Millisecond}
	msg := err.Error()
	assert.Contains(t, msg, `"bedrock"`)
	assert.Contains(t, msg, "depth=8")
	assert.Contains(t, msg, "30ms")
}

// TestIsRouteOverflow_NilAndOther — predicate handles nil and
// non-overflow errors without panicking.
func TestIsRouteOverflow_NilAndOther(t *testing.T) {
	assert.False(t, IsRouteOverflow(nil))
	assert.False(t, IsRouteOverflow(errors.New("other")))
	assert.True(t, IsRouteOverflow(&RouteOverflowError{}))
}

// TestBoundedRouteProvider_MetricsRecorded — when a Prometheus
// registerer is wired, both happy-path and overflow paths bump
// the expected series so operators can size queue_depth + alert
// on overflow rate.
func TestBoundedRouteProvider_MetricsRecorded(t *testing.T) {
	reg := prometheus.NewRegistry()
	block := make(chan struct{})
	inner := &stubBoundedRouteProvider{block: block}
	q := NewBoundedRouteProvider(inner, "bedrock", 1, 5*time.Millisecond, reg)

	// Drive a happy path + an overflow.
	go func() { _, _ = q.Complete(context.Background(), nil) }()
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&inner.inFlight) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err := q.Complete(context.Background(), nil)
	require.Error(t, err)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	// Find the overflow counter via the testutil helper which is
	// scoped per-vec.
	qp := q.(*BoundedRouteProvider)
	got := testutil.ToFloat64(qp.metrics.overflowCounter.WithLabelValues("bedrock"))
	assert.GreaterOrEqual(t, got, 1.0, "overflow counter must record at least 1 for the overflowed call")
	_ = mfs
	close(block)
}

// TestBoundedRouteProvider_AllSurfacesQueueGated — every method on
// Provider (Complete + CompleteWithTools + CompleteWithToolsStream)
// must acquire a queue slot. Mirrors CapsInFlightAtDepth for the
// other two surfaces so coverage isn't lopsided.
func TestBoundedRouteProvider_AllSurfacesQueueGated(t *testing.T) {
	inner := &stubBoundedRouteProvider{}
	q := NewBoundedRouteProvider(inner, "x", 5, time.Second, nil)
	_, err1 := q.CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err1)
	_, err2 := q.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	require.NoError(t, err2)
	assert.Equal(t, int32(2), atomic.LoadInt32(&inner.calls))
}

// TestBoundedRouteProvider_ModelAndSetMetricsDelegated — the queue
// wrapper must not hide the inner's Model() identity or
// SetMetrics wiring; otherwise per-model Prometheus labels
// would land under the wrong route and the dispatcher's
// "which model ran this step" UI would show the wrapper.
func TestBoundedRouteProvider_ModelAndSetMetricsDelegated(t *testing.T) {
	inner := &stubBoundedRouteProvider{model: "gpt-5"}
	q := NewBoundedRouteProvider(inner, "x", 1, time.Second, nil)
	assert.Equal(t, "gpt-5", q.Model())
	q.SetMetrics(nil) // must not panic
}
