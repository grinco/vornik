package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/ratelimit"
)

// TestPerIPLimit_BlocksPastBurst — hammering one IP past burst
// returns 429 with Retry-After; the counter fires. This mirrors
// TestAuthMiddleware_PerIP_BlocksUnauthenticatedFlood but exercises
// the exported PerIPLimit standalone middleware used on /auth/*.
func TestPerIPLimit_BlocksPastBurst(t *testing.T) {
	limiter := ratelimit.NewPerIPLimiter()
	metrics := ratelimit.NewMetrics(prometheus.NewRegistry())
	mw := PerIPLimit(limiter, metrics, 1, 2) // rps=1 burst=2

	reached := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})

	hit := func() *httptest.ResponseRecorder {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = "203.0.113.42:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		return rec
	}

	// Burst 2: first two calls pass through; the third is 429.
	rec1 := hit()
	require.Equal(t, http.StatusOK, rec1.Code, "first call should reach handler (200)")
	rec2 := hit()
	require.Equal(t, http.StatusOK, rec2.Code, "second call should reach handler (200)")
	rec3 := hit()
	require.Equal(t, http.StatusTooManyRequests, rec3.Code, "third call must be 429")
	assert.NotEmpty(t, rec3.Header().Get("Retry-After"), "must set Retry-After")
	assert.Contains(t, rec3.Body.String(), "per-IP")
	assert.Equal(t, 2, reached, "handler must not be reached on the blocked call")

	// Metrics: at least one block decision on scope=ip.
	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(ratelimit.ScopeIP, ratelimit.OutcomeBlock))
	assert.GreaterOrEqual(t, got, 1.0, "block decision must be recorded")
}

// TestPerIPLimit_DistinctIPsUnaffected — draining one IP's bucket
// must not affect other IPs. Mirrors
// TestAuthMiddleware_PerIP_DistinctIPsHaveIndependentBuckets.
func TestPerIPLimit_DistinctIPsUnaffected(t *testing.T) {
	limiter := ratelimit.NewPerIPLimiter()
	mw := PerIPLimit(limiter, nil, 1, 1) // burst=1

	hit := func(ip string) int {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = ip + ":5000"
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec.Code
	}

	// Drain IP A — second call 429.
	assert.Equal(t, http.StatusOK, hit("203.0.113.10"), "IP A first call passes")
	assert.Equal(t, http.StatusTooManyRequests, hit("203.0.113.10"), "IP A second call 429")

	// IP B still has its full burst.
	assert.Equal(t, http.StatusOK, hit("203.0.113.99"), "IP B unaffected by IP A drain")
}

// TestPerIPLimit_NilLimiterPassThrough — nil limiter is a no-op;
// the middleware acts as a transparent pass-through without 429s.
func TestPerIPLimit_NilLimiterPassThrough(t *testing.T) {
	// nil limiter → disabled.
	mw := PerIPLimit(nil, nil, 1, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 50; i++ {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = "203.0.113.42:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "nil limiter: all calls must reach handler, call %d", i)
	}
}

// TestPerIPLimit_ZeroRPSPassThrough — zero rps/burst is a no-op;
// matches the AuthMiddleware nil-limiter contract.
func TestPerIPLimit_ZeroRPSPassThrough(t *testing.T) {
	limiter := ratelimit.NewPerIPLimiter()
	mw := PerIPLimit(limiter, nil, 0, 0)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 50; i++ {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = "203.0.113.42:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "zero rps: all calls must reach handler, call %d", i)
	}
}

// TestPerIPLimit_ForgedHeaderCannotEvadeBucket is the end-to-end consumer
// regression for the Cloudflare tunnel real-IP spoof: leftmost-XFF was
// attacker-controllable. Composing the outermost realip.Middleware (the
// production wiring) with the per-IP limiter, an attacker on a SINGLE
// untrusted RemoteAddr who rotates a forged CF-Connecting-IP / XFF header
// on every request must STILL be collapsed to one bucket — the forged
// header cannot move the rate-limit key, so it cannot be used to evade the
// limit. Pre-fix the limiter read leftmost-XFF and every rotated value got
// a fresh full bucket.
func TestPerIPLimit_ForgedHeaderCannotEvadeBucket(t *testing.T) {
	// realip trusts ONLY the cloudflared host 10.0.0.5; our attacker is
	// not it.
	rcfg, err := realip.NewConfig(true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	require.NoError(t, err)

	limiter := ratelimit.NewPerIPLimiter()
	perIP := PerIPLimit(limiter, nil, 1, 2) // burst=2
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Production order: realip outermost, then the limiter.
	mw := realip.Middleware(rcfg, nil)(perIP(handler))

	hit := func(forged string) int {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = "198.51.100.9:5000" // untrusted attacker peer
		req.Header.Set("CF-Connecting-IP", forged)
		req.Header.Set("X-Forwarded-For", forged)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec.Code
	}

	// Burst 2 against the SINGLE real key (the attacker's RemoteAddr),
	// despite a different forged IP each call.
	assert.Equal(t, http.StatusOK, hit("203.0.113.1"))
	assert.Equal(t, http.StatusOK, hit("203.0.113.2"))
	assert.Equal(t, http.StatusTooManyRequests, hit("203.0.113.3"),
		"rotating the forged header must NOT grant a fresh bucket")
}

// TestPerIPLimit_TrustedProxyHeaderSeparatesClients — the flip side: when
// the request genuinely arrives from the trusted cloudflared host, the
// per-IP key follows the trusted CF-Connecting-IP, so distinct real
// clients behind the tunnel get independent buckets and one bad actor can't
// lock everyone out.
func TestPerIPLimit_TrustedProxyHeaderSeparatesClients(t *testing.T) {
	rcfg, err := realip.NewConfig(true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	require.NoError(t, err)

	limiter := ratelimit.NewPerIPLimiter()
	perIP := PerIPLimit(limiter, nil, 1, 1) // burst=1
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := realip.Middleware(rcfg, nil)(perIP(handler))

	hit := func(realClient string) int {
		req := newAuthRequest(t, "/auth/github/start")
		req.RemoteAddr = "10.0.0.5:5000" // trusted cloudflared host
		req.Header.Set("CF-Connecting-IP", realClient)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec.Code
	}

	// Client A drains its burst-1 bucket.
	assert.Equal(t, http.StatusOK, hit("203.0.113.10"))
	assert.Equal(t, http.StatusTooManyRequests, hit("203.0.113.10"))
	// Client B (different real client, same proxy) is unaffected.
	assert.Equal(t, http.StatusOK, hit("203.0.113.99"))
}
