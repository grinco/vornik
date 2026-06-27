package chat

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RouteOverflowError is returned when a per-route queue rejects
// a call because its bounded capacity is full AND the configured
// wait timeout elapsed before a slot opened. Distinct from an
// upstream 429: the daemon decided to refuse, not the provider.
// The chat-proxy surfaces this as HTTP 503 so callers
// distinguish "we pushed back" from "the upstream pushed back".
type RouteOverflowError struct {
	// Route is the operator-visible label of the route that
	// overflowed ("bedrock" / "anthropic-subscription" / etc).
	// Carried in the error message AND on the struct so callers
	// reading the error programmatically can switch on it.
	Route string
	// Depth is the configured queue depth at refusal time —
	// surfaced in the error message so the operator immediately
	// sees the cap they hit.
	Depth int
	// Waited is how long the call waited before giving up.
	// Useful for tuning QueueTimeoutMs: if Waited is consistently
	// equal to the timeout, the upstream is too slow OR the depth
	// is too small.
	Waited time.Duration
}

// Error implements the error interface.
func (e *RouteOverflowError) Error() string {
	return fmt.Sprintf("chat route %q queue full (depth=%d, waited=%s)",
		e.Route, e.Depth, e.Waited)
}

// IsRouteOverflow reports whether err is a *RouteOverflowError —
// callers use this in lieu of a type assertion when they want the
// boolean predicate.
func IsRouteOverflow(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*RouteOverflowError)
	return ok
}

// routeQueueMetrics holds the Prometheus instrumentation for the
// per-route queue layer. Cardinality is bounded by the number of
// configured routes (≤10 in any plausible deployment); operators
// scrape these to size the queue depth appropriately for their
// traffic shape.
type routeQueueMetrics struct {
	depthGauge       *prometheus.GaugeVec
	overflowCounter  *prometheus.CounterVec
	waitHistogramSec *prometheus.HistogramVec
}

// newRouteQueueMetrics registers the route-queue Prometheus
// surface. Returns nil if the registerer is nil — keeps the
// wrapper usable in tests that don't want Prometheus state.
func newRouteQueueMetrics(reg prometheus.Registerer) *routeQueueMetrics {
	if reg == nil {
		return nil
	}
	// Guard against a TYPED-NIL registerer: a nil *prometheus.Registry
	// smuggled through the prometheus.Registerer interface is a non-nil
	// interface value wrapping a nil pointer, so the `reg == nil` check
	// above misses it and promauto.With(reg).NewGaugeVec → (*Registry)(nil)
	// .MustRegister segfaults. The service's observabilityRegistry()
	// returns exactly this during early startup (observability inits
	// after chat), which crash-looped the daemon on 2026-06-03 the first
	// time a route set queue_depth>0. Treat a nil underlying registry as
	// "no metrics" — the queue still throttles.
	if r, ok := reg.(*prometheus.Registry); ok && r == nil {
		return nil
	}
	return &routeQueueMetrics{
		depthGauge: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "chat_route",
			Name:      "queue_depth",
			Help:      "Current in-flight + queued call count per chat route. Drives operator tuning of the route queue_depth config.",
		}, []string{"route"}),
		overflowCounter: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat_route",
			Name:      "overflow_total",
			Help:      "RouteOverflowError count by route. A non-zero rate is the operator signal to raise queue_depth or scale the upstream.",
		}, []string{"route"}),
		waitHistogramSec: promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "chat_route",
			Name:      "wait_seconds",
			Help:      "Wall time a call spent waiting for a queue slot before reaching the upstream. p99 climbing toward queue_timeout_ms is the early signal of saturation.",
			Buckets:   []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
		}, []string{"route"}),
	}
}

// BoundedRouteProvider wraps a chat.Provider with a bounded
// in-process queue — sub-item 4 of the rate-limit hardening
// track. Each call (Complete / CompleteWithTools /
// CompleteWithToolsStream) acquires a slot from a depth-N
// semaphore before reaching the upstream; when the semaphore
// is full AND no slot opens within the configured timeout, the
// call returns a *RouteOverflowError that the chat-proxy maps
// to HTTP 503.
//
// Goal: smooth autonomy bursts so we don't slam the provider
// AND keep the daemon's memory bounded — an unbounded queue
// would blow up under sustained load.
type BoundedRouteProvider struct {
	inner   Provider
	name    string // operator-visible label, stamped into RouteOverflowError
	sem     chan struct{}
	depth   int
	timeout time.Duration
	metrics *routeQueueMetrics
}

// NewBoundedRouteProvider wraps inner with a bounded per-route queue.
// depth ≤ 0 or inner == nil short-circuits — the function returns
// inner unwrapped so the call site can apply the queue layer
// uniformly without conditionally allocating. timeout ≤ 0 means
// "wait indefinitely" (only safe when caller timeouts dominate).
// name is purely decorative — surfaces in RouteOverflowError and
// Prometheus labels.
func NewBoundedRouteProvider(inner Provider, name string, depth int, timeout time.Duration, reg prometheus.Registerer) Provider {
	if inner == nil || depth <= 0 {
		return inner
	}
	return &BoundedRouteProvider{
		inner:   inner,
		name:    name,
		sem:     make(chan struct{}, depth),
		depth:   depth,
		timeout: timeout,
		metrics: newRouteQueueMetrics(reg),
	}
}

// acquire blocks until a queue slot opens, the context cancels,
// or the wait timeout elapses. Returns a release func the caller
// MUST defer; release is a no-op when err != nil.
func (q *BoundedRouteProvider) acquire(ctx context.Context) (release func(), err error) {
	start := time.Now()
	waitCtx := ctx
	var cancel context.CancelFunc = func() {}
	if q.timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, q.timeout)
	}
	defer cancel()
	select {
	case q.sem <- struct{}{}:
		waited := time.Since(start)
		if q.metrics != nil {
			q.metrics.depthGauge.WithLabelValues(q.name).Set(float64(len(q.sem)))
			q.metrics.waitHistogramSec.WithLabelValues(q.name).Observe(waited.Seconds())
		}
		return func() {
			<-q.sem
			if q.metrics != nil {
				q.metrics.depthGauge.WithLabelValues(q.name).Set(float64(len(q.sem)))
			}
		}, nil
	case <-waitCtx.Done():
		waited := time.Since(start)
		if q.metrics != nil {
			q.metrics.overflowCounter.WithLabelValues(q.name).Inc()
		}
		// Distinguish caller-cancelled (return their error) from
		// timeout-exceeded (return overflow). Caller cancellation
		// is normal shutdown; overflow is the daemon refusing to
		// queue further.
		if ctx.Err() != nil {
			return func() {}, ctx.Err()
		}
		return func() {}, &RouteOverflowError{
			Route:  q.name,
			Depth:  q.depth,
			Waited: waited,
		}
	}
}

// Complete implements Provider with queue gating.
func (q *BoundedRouteProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return q.inner.Complete(ctx, messages)
}

// CompleteWithTools implements Provider with queue gating.
func (q *BoundedRouteProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return q.inner.CompleteWithTools(ctx, messages, tools)
}

// CompleteWithToolsStream implements Provider with queue gating.
func (q *BoundedRouteProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return q.inner.CompleteWithToolsStream(ctx, messages, tools, onText)
}

// Model delegates to the inner provider — queueing is invisible
// to the dispatcher's "which model ran this step" surface.
func (q *BoundedRouteProvider) Model() string { return q.inner.Model() }

// SetMetrics delegates to the inner provider so existing
// per-model Prometheus labels keep landing on the same counters.
// The queue layer's own metrics are registered separately at
// construction time and aren't affected by this call.
func (q *BoundedRouteProvider) SetMetrics(m *Metrics) { q.inner.SetMetrics(m) }

// WithModel forwards ModelOverridable to the inner provider so
// the queue wrapping survives per-request model overrides. The
// returned Provider is a fresh BoundedRouteProvider that SHARES the
// underlying semaphore — that's the whole point, otherwise per
// request overrides would each get their own queue and the
// route-level depth contract would break.
func (q *BoundedRouteProvider) WithModel(model string) Provider {
	mo, ok := q.inner.(ModelOverridable)
	if !ok {
		return q
	}
	return &BoundedRouteProvider{
		inner:   mo.WithModel(model),
		name:    q.name,
		sem:     q.sem,
		depth:   q.depth,
		timeout: q.timeout,
		metrics: q.metrics,
	}
}

// Compile-time conformance checks.
var (
	_ Provider         = (*BoundedRouteProvider)(nil)
	_ ModelOverridable = (*BoundedRouteProvider)(nil)
)
