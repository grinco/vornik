package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/ratelimit"
)

// TestAuthMiddleware_PerIP_BlocksUnauthenticatedFlood — the
// backstop fires BEFORE auth so an unauthenticated flood can't
// reach the auth path at all. Burst 2 → 3rd call from same IP
// is 429 with Retry-After regardless of auth state.
func TestAuthMiddleware_PerIP_BlocksUnauthenticatedFlood(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 2,
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	hit := func() *httptest.ResponseRecorder {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = "203.0.113.42:5000"
		// No Authorization header on purpose — the per-IP gate
		// must reject without consulting auth.
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		return rec
	}

	// Burst 2: first two calls reach the auth path (which 401s
	// because no API key was provided). The 3rd is 429.
	rec1 := hit()
	require.Equal(t, http.StatusUnauthorized, rec1.Code, "first call should reach auth and 401")
	rec2 := hit()
	require.Equal(t, http.StatusUnauthorized, rec2.Code, "second call should reach auth and 401")
	rec3 := hit()
	require.Equal(t, http.StatusTooManyRequests, rec3.Code, "third call should 429 on per-IP backstop")
	assert.NotEmpty(t, rec3.Header().Get("Retry-After"))
	assert.Contains(t, rec3.Body.String(), "per-IP")
}

func TestAuthMiddleware_PerIP_AllowsNilMetrics(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 1,
		// RateLimitMetrics intentionally nil.
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for i, want := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = "203.0.113.42:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equalf(t, want, rec.Code, "call %d", i)
	}
}

// TestAuthMiddleware_PerIP_DistinctIPsHaveIndependentBuckets —
// two attackers from different IPs each get their own burst;
// one draining doesn't affect the other.
func TestAuthMiddleware_PerIP_DistinctIPsHaveIndependentBuckets(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 1,
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	drainAndExceed := func(ip string) int {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = ip + ":5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		return rec.Code
	}

	// Drain IP A — second call 429.
	assert.Equal(t, http.StatusUnauthorized, drainAndExceed("203.0.113.10"))
	assert.Equal(t, http.StatusTooManyRequests, drainAndExceed("203.0.113.10"))

	// IP B still has its full burst.
	assert.Equal(t, http.StatusUnauthorized, drainAndExceed("203.0.113.99"))
}

// TestAuthMiddleware_PerIP_HealthEndpointsExempt — /healthz must
// bypass the limiter even on a saturated bucket; otherwise a
// flood could black-hole readiness probes and trigger LB
// failover.
func TestAuthMiddleware_PerIP_HealthEndpointsExempt(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 1,
	})
	reached := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 10; i++ {
		req := newAuthRequest(t, "/healthz")
		req.RemoteAddr = "203.0.113.5:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "/healthz must always pass — request %d", i)
	}
	assert.Equal(t, 10, reached, "all 10 health probes should reach the handler")
}

// TestAuthMiddleware_PerIP_MetricsObservedOnBlock — the per-IP
// 429 path bumps the vornik_ratelimit_decisions_total counter on
// scope=ip / outcome=block. Operators need this label to split
// "unauthenticated flood" from per-key throttle on dashboards.
func TestAuthMiddleware_PerIP_MetricsObservedOnBlock(t *testing.T) {
	metrics := ratelimit.NewMetrics(prometheus.NewRegistry())
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 1,
		RateLimitMetrics:    metrics,
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for i := 0; i < 3; i++ {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = "203.0.113.5:5000"
		mw(handler).ServeHTTP(httptest.NewRecorder(), req)
	}

	// Burst 1: first call may record a warn (bucket emptied on the first token),
	// second and third are blocked.
	got := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(ratelimit.ScopeIP, ratelimit.OutcomeBlock))
	assert.GreaterOrEqual(t, got, 1.0, "at least one block decision must be recorded on scope=ip")
}

// TestAuthMiddleware_PerIP_DisabledLeavesAuthIntact — when
// limiter is nil OR rps/burst are zero, the gate is a no-op and
// the existing auth path runs unchanged.
func TestAuthMiddleware_PerIP_DisabledLeavesAuthIntact(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled: true,
		// No PerIPLimiter wired.
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for i := 0; i < 100; i++ {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = "203.0.113.5:5000"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "no per-IP limit configured — should always reach auth and 401")
	}
}

// TestAuthMiddleware_PerIP_UnparseableRemoteAddrStillBuckets —
// after the realip centralisation the limiter keys on the raw
// RemoteAddr string when it can't be parsed to an IP (realip.RemoteHost
// passes the value through verbatim). A malformed peer address therefore
// still gets throttled by its (degenerate) key rather than slipping past
// the limiter — one bad peer = one bucket, which is the safer posture for
// a network-reachable daemon. The first request passes the burst-1
// bucket, subsequent ones 429.
func TestAuthMiddleware_PerIP_UnparseableRemoteAddrStillBuckets(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:             true,
		PerIPLimiter:        ratelimit.NewPerIPLimiter(),
		PerIPRateLimitRPS:   1,
		PerIPRateLimitBurst: 1,
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	codes := map[int]int{}
	for i := 0; i < 50; i++ {
		req := newAuthRequest(t, "/api/v1/projects/p1/tasks")
		req.RemoteAddr = "notanip"
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		codes[rec.Code]++
	}
	require.Equal(t, 1, codes[http.StatusUnauthorized], "first request drains the burst-1 bucket and reaches auth (401)")
	require.Equal(t, 49, codes[http.StatusTooManyRequests], "subsequent requests on the same key are throttled")
}
