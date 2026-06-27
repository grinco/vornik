package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the latency-relevant retry/queue behaviour that landed
// across commits e7403214 → ec6e8176 (client retries 429 ONLY, NOT 5xx —
// preserving the dispatcher's context-pruning prune-and-retry for 5xx) and
// the OpenRouter free-tier bounded queue (queue_depth / queue_timeout_ms).
//
// The existing sibling tests cover adjacent ground (see http_retry_test.go's
// helper-level 429 cases and route_queue_test.go's depth=1/3 overflow cases),
// so the cases here deliberately target the GAPS:
//
//   - the END-TO-END *Client* contract that 5xx is hit exactly once (NOT
//     retried at the client layer) while 429 IS retried and bounded by
//     maxAttempts — the existing TestDoComplete_Returns5xxAsGatewayError
//     asserts the error SHAPE but never the attempt COUNT, so a regression
//     that re-enabled client-side 5xx retry would slip through;
//   - the backoff is BOUNDED (caps at ~8s, never unbounded) via the helper's
//     injected clock;
//   - the OpenRouter free-tier queue (depth=4) makes the 5th concurrent
//     request WAIT, and TIMES OUT into a *RouteOverflowError rather than
//     hanging forever when queue_timeout_ms elapses.
//
// Helpers/types added here are prefixed `rq` per the package collision rule.

// rqHitCountServer returns an httptest server that always replies with the
// given status+body and atomically counts how many times it was hit. The
// counter is the load-bearing assertion: it proves whether the client layer
// retried (count>1) or surfaced on the first try (count==1).
func rqHitCountServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// rqInstantSleep is a sleepFn for withRetryClock that records the durations
// the retry loop WOULD have slept on and returns instantly, so the tests are
// deterministic (no real backoff wall-time) yet can assert the bound.
func rqInstantSleep(rec *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*rec = append(*rec, d)
		return nil
	}
}

// --- Client-level: 5xx is NOT retried (preserve dispatcher prune-retry) ---

// TestRQClient_5xxNotRetried_SurfacesOnFirstAttempt is the ec6e8176 contract:
// the OpenAI-compat client opts into withNo5xxRetry(), so a 5xx must surface
// to the caller as a *GatewayError on the FIRST attempt — the daemon relies on
// the dispatcher's context-pruning prune-and-retry to recover from 5xx (a
// blind client retry of the same bloated history would just fail again, and
// burns latency). The existing TestDoComplete_Returns5xxAsGatewayError checks
// the error shape but never the hit count; this nails the count to 1.
func TestRQClient_5xxNotRetried_SurfacesOnFirstAttempt(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"500 internal", http.StatusInternalServerError},
		{"502 bad gateway", http.StatusBadGateway},
		{"503 unavailable", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := rqHitCountServer(t, tc.status, `{"error":{"message":"upstream down"}}`)
			c := NewClient(srv.URL, "k", "m")

			_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
			require.Error(t, err)

			var g *GatewayError
			require.True(t, errors.As(err, &g), "5xx must surface as *GatewayError; got %T", err)
			assert.Equal(t, tc.status, g.Status)
			assert.True(t, g.Retryable(), "5xx GatewayError must report Retryable() so the dispatcher prunes+retries")
			assert.Equal(t, int32(1), hits.Load(),
				"5xx must NOT be retried at the client layer (withNo5xxRetry) — exactly one upstream hit")
		})
	}
}

// --- Client-level: 429 IS retried and bounded by maxAttempts ---

// TestRQClient_429RetriedThenSucceeds proves the inverse of the 5xx case: a
// 429 (transient rate limit, which pruning can't help) IS retried at the
// client layer (withRetryOn429 + withGenericBackoffOn429). A first-hit 429
// with no Retry-After header (the Vertex / OpenRouter RESOURCE_EXHAUSTED
// shape) falls back to bounded backoff and the second attempt succeeds.
func TestRQClient_429RetriedThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// No Retry-After header → exercises the generic-backoff path.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	resp, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err, "429 followed by 200 must succeed via client-level retry")
	require.NotNil(t, resp)
	assert.Equal(t, int32(2), hits.Load(), "expected one 429 then one 200 = 2 upstream hits")
}

// TestRQClient_429RetriesBoundedByMaxAttempts proves the retry budget is
// BOUNDED: a server that 429s forever must not loop indefinitely. The client
// pins maxAttempts=3, so the upstream is hit at most 3 times before the 429
// surfaces as a *GatewayError. This is the latency guardrail — an unbounded
// retry loop on a persistently rate-limited upstream would hang the request.
func TestRQClient_429RetriesBoundedByMaxAttempts(t *testing.T) {
	srv, hits := rqHitCountServer(t, http.StatusTooManyRequests, `{"error":{"message":"always throttled"}}`)
	c := NewClient(srv.URL, "k", "m")

	start := time.Now()
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	elapsed := time.Since(start)
	require.Error(t, err)

	var g *GatewayError
	require.True(t, errors.As(err, &g), "exhausted 429 retries must surface as *GatewayError; got %T", err)
	assert.Equal(t, http.StatusTooManyRequests, g.Status)
	// maxAttempts=3 in doComplete → at most 3 upstream hits. Bounded, not infinite.
	assert.Equal(t, int32(3), hits.Load(), "429 retry must be bounded by maxAttempts=3")
	// Sanity ceiling on wall time: 3 attempts with base 500ms backoff and a
	// single backoff between attempts can't approach the indefinite case.
	assert.Less(t, elapsed, 30*time.Second, "bounded retry must not hang")
}

// --- Helper-level: retry fires on 429 ONLY, not 5xx, with all three client opts ---

// TestRQHelper_ClientOptionCombo_429OnlyNot5xx exercises retryableHTTPDo with
// the EXACT option triple the OpenAI-compat client passes
// (withRetryOn429 + withGenericBackoffOn429 + withNo5xxRetry) and asserts the
// split directly at the helper boundary, using an injected clock so backoff is
// instant:
//   - a 5xx is handed straight back to the caller (NOT retried) → 1 hit;
//   - a 429 IS retried up to maxAttempts → bounded hit count.
func TestRQHelper_ClientOptionCombo_429OnlyNot5xx(t *testing.T) {
	clientOpts := []retryOption{withRetryOn429(nil), withGenericBackoffOn429(), withNo5xxRetry()}

	t.Run("5xx returned to caller, not retried", func(t *testing.T) {
		srv, hits := rqHitCountServer(t, http.StatusBadGateway, "boom")
		var sleeps []time.Duration
		opts := append([]retryOption{withRetryClock(nil, rqInstantSleep(&sleeps))}, clientOpts...)

		resp, err := retryableHTTPDo(
			context.Background(), &http.Client{},
			func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
			3, 500*time.Millisecond, zerolog.Nop(), opts...,
		)
		// withNo5xxRetry hands a 5xx back via the default branch — caller gets
		// the raw response (no error), so the *Client* turns it into a
		// GatewayError downstream. Crucially, only one upstream hit + no sleeps.
		require.NoError(t, err)
		require.NotNil(t, resp)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.Equal(t, int32(1), hits.Load(), "5xx must NOT be retried when withNo5xxRetry is set")
		assert.Empty(t, sleeps, "no backoff sleeps on a non-retried 5xx")
	})

	t.Run("429 retried up to maxAttempts then surfaces", func(t *testing.T) {
		srv, hits := rqHitCountServer(t, http.StatusTooManyRequests, "throttled")
		var sleeps []time.Duration
		opts := append([]retryOption{withRetryClock(nil, rqInstantSleep(&sleeps))}, clientOpts...)

		const maxAttempts = 3
		_, err := retryableHTTPDo(
			context.Background(), &http.Client{},
			func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
			maxAttempts, 500*time.Millisecond, zerolog.Nop(), opts...,
		)
		require.Error(t, err)
		var rerr *retryableHTTPError
		require.True(t, errors.As(err, &rerr), "want *retryableHTTPError; got %T", err)
		assert.Equal(t, http.StatusTooManyRequests, rerr.StatusCode)
		assert.Equal(t, int32(maxAttempts), hits.Load(), "429 retry bounded by maxAttempts")
		// One backoff sleep between each pair of attempts → maxAttempts-1.
		assert.Len(t, sleeps, maxAttempts-1, "exactly maxAttempts-1 backoff sleeps")
	})
}

// TestRQHelper_BackoffIsBounded proves the backoff doubles but is CAPPED at
// ~8s — it never grows unbounded across a long max-attempts loop. We drive a
// 5xx with the default (retryable) 5xx path and a large maxAttempts, capturing
// the sleep durations via the injected clock; every recorded sleep must be
// ≤ 8s and the sequence must stop doubling once it hits the cap.
func TestRQHelper_BackoffIsBounded(t *testing.T) {
	srv, _ := rqHitCountServer(t, http.StatusInternalServerError, "down")
	var sleeps []time.Duration

	const maxAttempts = 8
	_, err := retryableHTTPDo(
		context.Background(), &http.Client{},
		func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
		maxAttempts, 500*time.Millisecond, zerolog.Nop(),
		withRetryClock(nil, rqInstantSleep(&sleeps)),
	)
	require.Error(t, err)
	require.Len(t, sleeps, maxAttempts-1, "one backoff sleep between each attempt")

	for i, d := range sleeps {
		assert.LessOrEqual(t, d, 8*time.Second, "backoff[%d]=%v must be capped at 8s", i, d)
	}
	// The first few sleeps double (500ms, 1s, 2s, 4s, 8s) then PIN at 8s — the
	// last sleep must equal the cap, proving the bound actually engages rather
	// than the loop merely being short.
	assert.Equal(t, 8*time.Second, sleeps[len(sleeps)-1], "tail of a long loop must sit at the 8s cap")
}

// --- OpenRouter free-tier bounded queue: depth + timeout ---

// rqBlockingProvider is a minimal Provider that parks every call on a release
// channel so a test can hold queue slots open deterministically, then count
// how many calls actually reached the inner. It is a free-tier-shaped stand-in
// for *Client behind NewBoundedRouteProvider — distinct from the sibling
// stubBoundedRouteProvider so the two files don't collide.
type rqBlockingProvider struct {
	release  chan struct{}
	inFlight atomic.Int32
	reached  atomic.Int32
}

func (p *rqBlockingProvider) hit(ctx context.Context) (*ChatResponse, error) {
	p.reached.Add(1)
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	select {
	case <-p.release:
		return &ChatResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *rqBlockingProvider) Complete(ctx context.Context, _ []Message) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *rqBlockingProvider) CompleteWithTools(ctx context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *rqBlockingProvider) CompleteWithToolsStream(ctx context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	return p.hit(ctx)
}
func (p *rqBlockingProvider) Model() string         { return "deepseek/deepseek-r1:free" }
func (p *rqBlockingProvider) SetMetrics(_ *Metrics) {}

// rqWaitForInFlight spins (bounded) until at least n calls are parked inside
// the inner provider, so the test doesn't race the goroutines that fill the
// queue. Fails the test if the count isn't reached within the deadline.
func rqWaitForInFlight(t *testing.T, p *rqBlockingProvider, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.inFlight.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, p.inFlight.Load(), n, "expected %d calls in-flight inside the inner provider", n)
}

// TestRQFreeTierQueue_DepthBlocksAndTimesOut models the OpenRouter free-tier
// route exactly as config.yaml wires it: queue_depth=4 with a finite
// queue_timeout_ms. The first 4 concurrent calls fill the depth-4 semaphore;
// the 5th must WAIT (the queue is full) and, because no slot opens before the
// timeout elapses, TIME OUT into a *RouteOverflowError rather than hanging
// forever. This is the free-tier-specific latency guarantee: a burst beyond
// depth is shed with a 503-mappable error, not silently parked indefinitely.
func TestRQFreeTierQueue_DepthBlocksAndTimesOut(t *testing.T) {
	const depth = 4
	// Short timeout stands in for the configured queue_timeout_ms so the test
	// is fast and deterministic; the contract (wait → overflow) is identical.
	const timeout = 40 * time.Millisecond

	inner := &rqBlockingProvider{release: make(chan struct{})}
	q := NewBoundedRouteProvider(inner, "openrouter", depth, timeout, nil)

	// Fill all `depth` slots with parked calls.
	for i := 0; i < depth; i++ {
		go func() { _, _ = q.CompleteWithTools(context.Background(), nil, nil) }()
	}
	rqWaitForInFlight(t, inner, depth)

	// The (depth+1)th call finds the queue full. It should block until the
	// timeout, then return a *RouteOverflowError — NOT hang and NOT reach the
	// inner provider.
	start := time.Now()
	_, err := q.CompleteWithTools(context.Background(), nil, nil)
	waited := time.Since(start)

	require.Error(t, err)
	var roe *RouteOverflowError
	require.True(t, errors.As(err, &roe), "request beyond depth must overflow; got %T (%v)", err, err)
	assert.Equal(t, "openrouter", roe.Route)
	assert.Equal(t, depth, roe.Depth, "overflow error must carry the configured queue_depth")
	assert.True(t, IsRouteOverflow(err))
	// It actually WAITED (didn't reject instantly) — proving the queue gives a
	// transient burst a chance to drain before shedding.
	assert.GreaterOrEqual(t, waited, timeout, "overflowing call must wait the full queue_timeout before giving up")
	// And it bounded the wait — it didn't hang far past the timeout.
	assert.Less(t, waited, 2*time.Second, "overflow must time out promptly, not hang indefinitely")
	// The overflowed call must NOT have reached the upstream.
	assert.Equal(t, int32(depth), inner.reached.Load(), "only depth calls may reach the upstream; the overflow is shed")

	// Release the parked calls so the goroutines exit cleanly.
	close(inner.release)
}

// TestRQFreeTierQueue_SlotFreesBeforeTimeoutAdmits is the positive counterpart:
// when a slot frees BEFORE queue_timeout_ms elapses, the waiting call is
// admitted rather than overflowed — the queue throttles, it doesn't reject
// prematurely. This guards against a regression that made the queue reject on
// fullness without honouring the wait window.
func TestRQFreeTierQueue_SlotFreesBeforeTimeoutAdmits(t *testing.T) {
	const depth = 2
	inner := &rqBlockingProvider{release: make(chan struct{})}
	// Generous timeout: a slot will free well before it elapses.
	q := NewBoundedRouteProvider(inner, "openrouter", depth, time.Hour, nil)

	// Fill both slots.
	for i := 0; i < depth; i++ {
		go func() { _, _ = q.Complete(context.Background(), nil) }()
	}
	rqWaitForInFlight(t, inner, depth)

	// Launch a 3rd caller that must wait for a slot.
	admitted := make(chan error, 1)
	go func() {
		_, err := q.Complete(context.Background(), nil)
		admitted <- err
	}()

	// Free exactly one slot. Releasing all parked calls lets one of the held
	// slots open; the waiter should acquire it and (since release is closed)
	// also complete.
	close(inner.release)

	select {
	case err := <-admitted:
		require.NoError(t, err, "a freed slot must admit the waiter, not overflow it")
		assert.False(t, IsRouteOverflow(err))
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never admitted after a slot freed — queue rejected prematurely or hung")
	}
}
