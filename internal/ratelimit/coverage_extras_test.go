package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// TestLimiter_CheckCtxRecordCtx_PassesThroughToCheck — the context-
// aware shims on the in-process Limiter delegate to the no-ctx
// methods; tests would otherwise miss them since most call sites
// use Check/Record directly. ctx is ignored but the signatures must
// remain wired so backend swaps stay transparent.
func TestLimiter_CheckCtxRecordCtx_PassesThroughToCheck(t *testing.T) {
	l := New()
	p := &registry.Project{
		ID:        "p1",
		RateLimit: registry.ProjectRateLimit{TasksPerMinute: 2},
	}
	now := time.Unix(1700000000, 0)
	ctx := context.Background()

	d := l.CheckCtx(ctx, p, now)
	if d.Blocked {
		t.Errorf("CheckCtx on fresh limiter blocked: %+v", d)
	}
	l.RecordCtx(ctx, "p1", now)
	l.RecordCtx(ctx, "p1", now)

	d = l.CheckCtx(ctx, p, now)
	if !d.Blocked {
		t.Errorf("CheckCtx after 2 RecordCtx calls should block on minute cap=2: %+v", d)
	}
}

// TestAPIKeyLimiter_Forget_NilAndEmptySafe — both nil-receiver and
// empty-keyID Forget short-circuit. Mirrors Allow's nil-safety; the
// existing tests only exercise the non-empty success path.
func TestAPIKeyLimiter_Forget_NilAndEmptySafe(t *testing.T) {
	var nilL *APIKeyLimiter
	nilL.Forget("k") // must not panic

	l := NewAPIKeyLimiter()
	l.Forget("") // empty key short-circuits before the map delete
	// Still functional after the no-op.
	d := l.Allow("k2", 10, 1, time.Now())
	if d.Blocked {
		t.Errorf("Forget('') should not affect unrelated buckets: %+v", d)
	}
}

// TestKeyBucket_RefillCapsAtBurst — sustained idle time would
// otherwise let `tokens += elapsed*rps` grow unbounded; the cap at
// burst is the safety net the refill helper enforces. Drives a
// bucket through a long idle window and asserts tokens stay at
// burst.
func TestKeyBucket_RefillCapsAtBurst(t *testing.T) {
	l := NewAPIKeyLimiter()
	now := time.Unix(1700000000, 0)
	// Prime the bucket and consume one token so refill has work to do.
	d := l.Allow("k-cap", 10, 5, now)
	if d.RemainingTokens > 4.001 || d.RemainingTokens < 3.999 {
		t.Fatalf("after one consume remaining=%.3f, want 4", d.RemainingTokens)
	}
	// 1 hour later — would add 36000 tokens unclamped; must cap at burst=5.
	future := now.Add(time.Hour)
	d = l.Allow("k-cap", 10, 5, future)
	// Consumed one of the (capped) refilled tokens, so 5 → 4 remaining.
	if d.RemainingTokens > 4.001 || d.RemainingTokens < 3.999 {
		t.Errorf("after long-idle refill+consume remaining=%.3f, want ≈4 (burst cap)", d.RemainingTokens)
	}
	if d.Blocked {
		t.Errorf("idle refill should not block: %+v", d)
	}
}

// TestMetrics_RecordEvent_NilAndEmptySafe — recordEvent fires from
// Observe / ObserveProject under degradation; both nil receiver and
// empty scope/id short-circuit so observability traffic on an
// unconfigured limiter never panics.
func TestMetrics_RecordEvent_NilAndEmptySafe(t *testing.T) {
	var nilM *Metrics
	nilM.recordEvent("api_key", "k", true) // must not panic

	m := newTestMetrics(t)
	m.recordEvent("", "k", true)       // empty scope short-circuits
	m.recordEvent("api_key", "", true) // empty id short-circuits
	// Sanity: confirm nothing was recorded under either empty input.
	summary := m.StatusFor("api_key", "k")
	if summary.RecentWarns != 0 || summary.RecentBlocks != 0 {
		t.Errorf("recordEvent with empty inputs leaked into status: %+v", summary)
	}
}

// TestMetrics_ObserveProject_NilSafe — nil receiver and empty
// projectID short-circuit. The active-receiver path is exercised
// elsewhere; this test pins the defensive top guard.
func TestMetrics_ObserveProject_NilSafe(t *testing.T) {
	var nilM *Metrics
	nilM.ObserveProject("p1", Decision{Blocked: true})

	m := newTestMetrics(t)
	m.ObserveProject("", Decision{Blocked: true}) // empty id short-circuits
}

// TestPerIPLimiter_NilReceiver_ZeroValueReturns — every public
// method nil-guards so a daemon configured without the limiter
// stays callable rather than panicking on the first request.
func TestPerIPLimiter_NilReceiver_ZeroValueReturns(t *testing.T) {
	var l *PerIPLimiter
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	d, ip := l.Allow(req, 1, 1, time.Now())
	if d.Blocked || ip != "" {
		t.Errorf("nil limiter Allow = (%+v, %q), want zero", d, ip)
	}
	if got := l.ClientIP(req); got != "" {
		t.Errorf("nil limiter ClientIP = %q, want empty", got)
	}
	if _, ok := l.SnapshotFor("1.2.3.4"); ok {
		t.Error("nil limiter SnapshotFor returned ok=true")
	}
}

// TestPerIPLimiter_HeaderIgnored_KeysOnRemoteAddr — trusted-proxy
// resolution moved to internal/httpx/realip; the limiter no longer
// reads any header. A request with a forwarding header but no
// context value keys on RemoteAddr's host.
func TestPerIPLimiter_HeaderIgnored_KeysOnRemoteAddr(t *testing.T) {
	l := NewPerIPLimiter()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:55555"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set("CF-Connecting-IP", "198.51.100.8")
	d, ip := l.Allow(req, 5, 1, time.Unix(1700000000, 0))
	if ip != "10.0.0.1" {
		t.Errorf("ClientIP via Allow = %q, want 10.0.0.1 (headers must be ignored)", ip)
	}
	if d.Blocked {
		t.Errorf("fresh bucket blocked: %+v", d)
	}
}

// TestPerIPLimiter_UnparseableRemoteAddr — when RemoteAddr can't be
// resolved to an IP and there is no context value, the limiter keys
// on the raw RemoteAddr string (realip.RemoteHost returns it
// verbatim). The Allow path still produces a non-empty key.
func TestPerIPLimiter_UnparseableRemoteAddr(t *testing.T) {
	l := NewPerIPLimiter()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "definitely-not-an-ip"
	if got := l.ClientIP(req); got != "definitely-not-an-ip" {
		t.Errorf("ClientIP = %q, want raw RemoteAddr passthrough", got)
	}
}

// newTestMetrics constructs a Metrics with a fresh prometheus
// registry — sharing the default registerer across tests panics on
// duplicate metric registration. Mirrors snapshot_test.go's
// newTestRegistry helper; pinning it to a t.Helper here keeps the
// coverage-extras tests self-contained.
func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return NewMetrics(newTestRegistry())
}
