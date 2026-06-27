package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
)

// TestAuthMiddleware_DBKey_RateLimited429WithRetryAfter — the
// headline contract: when a key's bucket is drained, the
// middleware returns 429 with a Retry-After header and the
// downstream handler is NOT invoked. Pre-fix a leaked / runaway
// key could hammer the daemon until revoked.
func TestAuthMiddleware_DBKey_RateLimited429WithRetryAfter(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	rps, burst := 2, 1 // single token; refills every 500ms
	row := &persistence.APIKey{
		ID:             "akey-rate",
		ProjectID:      "assistant",
		KeyHash:        apikey.Hash(key),
		CreatedAt:      time.Now(),
		RateLimitRPS:   &rps,
		RateLimitBurst: &burst,
	}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  &stubAPIKeyLookup{row: row},
		APIKeyLimiter: ratelimit.NewAPIKeyLimiter(),
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// First call drains the burst → 200.
	req1 := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req1.Header.Set("Authorization", "Bearer "+key)
	rec1 := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code, "first call should pass")

	// Second call must be rate-limited.
	req2 := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req2.Header.Set("Authorization", "Bearer "+key)
	rec2 := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, "second call should 429")
	ra := rec2.Header().Get("Retry-After")
	require.NotEmpty(t, ra, "Retry-After header missing")
	secs, err := strconv.Atoi(ra)
	require.NoError(t, err)
	require.True(t, secs >= 1, "retry_after should be at least 1 second; got %d", secs)
}

func TestAuthMiddleware_DBKey_RateLimitAllowsNilMetrics(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	rps, burst := 2, 1
	row := &persistence.APIKey{
		ID: "akey-rate-nil-metrics", ProjectID: "assistant",
		KeyHash: apikey.Hash(key), CreatedAt: time.Now(),
		RateLimitRPS: &rps, RateLimitBurst: &burst,
	}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  &stubAPIKeyLookup{row: row},
		APIKeyLimiter: ratelimit.NewAPIKeyLimiter(),
		// RateLimitMetrics intentionally nil.
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equalf(t, want, rec.Code, "call %d", i)
	}
}

// TestAuthMiddleware_DBKey_NoRateLimitConfiguredPasses — when a
// key has NULL rate_limit columns, the middleware MUST NOT
// allocate a bucket and the request passes regardless of
// throughput. Legacy keys (pre-rate-limit) work unchanged.
func TestAuthMiddleware_DBKey_NoRateLimitConfiguredPasses(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	row := &persistence.APIKey{
		ID: "akey-nolimit", ProjectID: "assistant",
		KeyHash: apikey.Hash(key), CreatedAt: time.Now(),
		// RateLimitRPS + RateLimitBurst BOTH nil.
	}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  &stubAPIKeyLookup{row: row},
		APIKeyLimiter: ratelimit.NewAPIKeyLimiter(),
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 10 calls in a row, all should pass.
	for i := 0; i < 10; i++ {
		req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "call %d unexpected status", i)
	}
}

// TestAuthMiddleware_DBKey_RateLimitEnforced_AuthDisabled — the
// rate limit is a property of the key, NOT of auth_enabled. Even
// with auth disabled (single-tenant local-dev), a key with
// configured limits must still throttle.
func TestAuthMiddleware_DBKey_RateLimitEnforced_AuthDisabled(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	rps, burst := 10, 1
	row := &persistence.APIKey{
		ID: "akey-x", ProjectID: "assistant",
		KeyHash: apikey.Hash(key), CreatedAt: time.Now(),
		RateLimitRPS: &rps, RateLimitBurst: &burst,
	}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       false, // disabled
		APIKeyLookup:  &stubAPIKeyLookup{row: row},
		APIKeyLimiter: ratelimit.NewAPIKeyLimiter(),
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req.Header.Set("Authorization", "Bearer "+key)
	rec1 := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec1, req)
	require.Equal(t, http.StatusOK, rec1.Code)

	req2 := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req2.Header.Set("Authorization", "Bearer "+key)
	rec2 := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusTooManyRequests, rec2.Code,
		"rate limit must enforce even when auth_enabled=false")
}

// TestAuthMiddleware_WarnTier_HeaderEmittedBefore429 — once the bucket
// crosses the WarnThresholdFrac line, the response carries
// X-Vornik-RateLimit-Warning so clients (and HA loops) can scale back
// without burning to the 429. The exact call where warn first fires
// is sensitive to wall-clock refill in between calls (rps=1 + several
// ms of test runtime = a fractional token), so we assert the property
// rather than a specific call index.
func TestAuthMiddleware_WarnTier_HeaderEmittedBefore429(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	rps, burst := 1, 5
	row := &persistence.APIKey{
		ID: "akey-warn", ProjectID: "assistant",
		KeyHash: apikey.Hash(key), CreatedAt: time.Now(),
		RateLimitRPS: &rps, RateLimitBurst: &burst,
	}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  &stubAPIKeyLookup{row: row},
		APIKeyLimiter: ratelimit.NewAPIKeyLimiter(),
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Drain the bucket and the calls that follow. Record per-call:
	// status code + whether the warn header fired.
	type observed struct {
		status int
		warn   string
	}
	observations := make([]observed, 0, 7)
	for i := 0; i < 7; i++ {
		req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		observations = append(observations, observed{
			status: rec.Code,
			warn:   rec.Header().Get("X-Vornik-RateLimit-Warning"),
		})
	}

	// Property 1: at least one call passes (200) with the warn header
	// — that's the early signal the operator/client needs.
	var sawPassWithWarn bool
	firstBlock := -1
	for i, o := range observations {
		if o.status == http.StatusOK && o.warn != "" {
			sawPassWithWarn = true
		}
		if o.status == http.StatusTooManyRequests && firstBlock < 0 {
			firstBlock = i
		}
	}
	require.True(t, sawPassWithWarn, "no 200-with-warn observed; warn header isn't reaching clients before 429: %+v", observations)
	require.GreaterOrEqual(t, firstBlock, 0, "expected at least one 429 in the burst window: %+v", observations)

	// Property 2: warn-header format includes threshold so operators
	// can read the limit from the response alone.
	for _, o := range observations {
		if o.warn != "" {
			require.True(t, strings.Contains(o.warn, "threshold="),
				"warn header must spell out the threshold: got %q", o.warn)
			break
		}
	}
}

// TestAuthMiddleware_RateLimitMetrics_RecordsAllowWarnBlock — the
// observability hardening surface. Operators read the per-outcome
// counter to alert on "block rate > X" across all keys without
// scanning the audit log. Exact counts of allow/warn depend on
// wall-clock refill timing (rps=1 + multi-ms test runtime), so we
// assert the qualitative property: all three outcomes appear and
// total to the call count.
func TestAuthMiddleware_RateLimitMetrics_RecordsAllowWarnBlock(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	rps, burst := 1, 5
	row := &persistence.APIKey{
		ID: "akey-metrics", ProjectID: "assistant",
		KeyHash: apikey.Hash(key), CreatedAt: time.Now(),
		RateLimitRPS: &rps, RateLimitBurst: &burst,
	}
	metrics := ratelimit.NewMetrics(prometheus.NewRegistry())
	mw := AuthMiddleware(AuthConfig{
		Enabled:          true,
		APIKeyLookup:     &stubAPIKeyLookup{row: row},
		APIKeyLimiter:    ratelimit.NewAPIKeyLimiter(),
		RateLimitMetrics: metrics,
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	const total = 7
	for i := 0; i < total; i++ {
		req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
	}

	allow := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeAllow))
	warn := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeWarn))
	block := testutil.ToFloat64(metrics.DecisionsTotal.WithLabelValues(ratelimit.ScopeAPIKey, ratelimit.OutcomeBlock))

	require.Equal(t, float64(total), allow+warn+block,
		"every call must produce exactly one outcome (allow|warn|block)")
	require.Greater(t, allow, 0.0, "at least one allow expected: a=%v w=%v b=%v", allow, warn, block)
	require.Greater(t, warn, 0.0, "at least one warn expected: a=%v w=%v b=%v", allow, warn, block)
	require.Greater(t, block, 0.0, "at least one block expected: a=%v w=%v b=%v", allow, warn, block)

	// Remaining-tokens gauge tracks the last decision's bucket level —
	// after the 7th rapid call the bucket is at or near empty.
	remaining := testutil.ToFloat64(metrics.RemainingTokens.WithLabelValues(ratelimit.ScopeAPIKey, "akey-metrics"))
	require.LessOrEqual(t, remaining, 1.0, "after drain the gauge should reflect near-empty bucket; got %v", remaining)
}
