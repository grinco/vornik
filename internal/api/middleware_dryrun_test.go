package api

// Tests for the dry-run evaluation mode (AuthConfig.DryRun=true).
//
// Dry-run is observation-only: every request is served exactly as if
// auth were disabled, but a per-verdict metric counter is incremented
// and a deduplicated warn log is emitted for each would-be denial.
//
// Test matrix:
//   01 — no credential + DryRun → served (200) AND denial recorded.
//   02 — valid static key + DryRun → served, NO denial recorded.
//   03 — two identical anonymous GETs → denial recorded TWICE (metric always increments).
//   04 — dead session cookie + DryRun → served, denial recorded.
//   05 — DryRun + Enabled both set → Enabled wins (defensive).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/auth"
)

// buildDryRunConfig returns an AuthConfig with DryRun=true and
// Enabled=false, wired with an optional static key map, an optional
// session backend, and a per-test Prometheus registry so the metric
// counter can be read back via testutil.ToFloat64.
func buildDryRunConfig(staticKeys map[string][]string, sessionBackend auth.Backend, m *DryRunMetrics) AuthConfig {
	cfg := AuthConfig{
		Enabled:       false,
		DryRun:        true,
		StaticAPIKeys: staticKeys,
		DryRunMetrics: m,
	}
	if sessionBackend != nil {
		cfg.SessionBackend = sessionBackend
	}
	return cfg
}

// newTestDryRunMetrics returns a DryRunMetrics backed by an isolated
// Prometheus registry so parallel tests don't collide on the default
// registerer and don't need cleanup.
func newTestDryRunMetrics() *DryRunMetrics {
	return NewDryRunMetrics(prometheus.NewRegistry())
}

// denialsFor reads the denial counter for a single verdict label.
func denialsFor(m *DryRunMetrics, verdict string) float64 {
	return testutil.ToFloat64(m.DenialsTotal.WithLabelValues(verdict))
}

// probeHandler responds 200 OK — simulates any real handler endpoint.
var probeHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// TestDryRun_NoCred_Served_AndDenialRecorded: anonymous POST to an API
// path is served (200) but a "missing_credential" denial is recorded.
func TestDryRun_NoCred_Served_AndDenialRecorded(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dry-run must serve)", rr.Code)
	}
	if got := denialsFor(m, dryRunVerdictMissingCred); got == 0 {
		t.Fatal("expected a dry-run denial to be recorded, got 0")
	}
	if got := denialsFor(m, dryRunVerdictMissingCred); got != 1 {
		t.Fatalf("missing_credential counter = %v, want 1", got)
	}
}

// TestDryRun_ValidStaticKey_Served_NoDenial: a request carrying a
// recognised static API key must pass with zero denials — the
// "would-be verdict" is pass.
func TestDryRun_ValidStaticKey_Served_NoDenial(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(map[string][]string{"good-key": nil}, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	for _, v := range []string{dryRunVerdictMissingCred, dryRunVerdictInvalidKey, dryRunVerdictDeadSession} {
		if got := denialsFor(m, v); got != 0 {
			t.Fatalf("expected 0 denials for valid key, got %v for verdict %q", got, v)
		}
	}
}

// TestDryRun_Dedup_TwoIdenticalAnonymousGETs: two identical
// anonymous GETs to the same path-shape — the metric increments on
// each request (metric always increments) so both fire a denial record.
func TestDryRun_Dedup_TwoIdenticalAnonymousGETs(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)

	fire := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := fire(); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code := fire(); code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", code)
	}
	// Both requests fire the metric (metric always increments regardless of dedup).
	if got := denialsFor(m, dryRunVerdictMissingCred); got < 2 {
		t.Fatalf("expected ≥2 metric increments (one per request), got %v", got)
	}
}

// TestDryRun_DeadSessionCookie_DenialRecorded: a request with a
// vornik_session cookie that does NOT resolve (dead/stale) should be
// served but a "dead_session" denial recorded — it would 401 when auth
// is enabled.
func TestDryRun_DeadSessionCookie_DenialRecorded(t *testing.T) {
	// stubSessionBackend that never recognises any token → ErrNoCredential.
	sb := &stubSessionBackend{token: "live-token"} // only "live-token" resolves
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, sb, m)

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "stale-dead-cookie"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dry-run serves everything)", rr.Code)
	}
	if got := denialsFor(m, dryRunVerdictDeadSession); got == 0 {
		t.Fatal("expected a dry-run denial for dead session cookie, got 0")
	}
}

// TestDryRun_EnabledWins: when both Enabled=true and DryRun=true are
// set (invalid config, but the middleware must be defensive), Enabled
// wins — auth is enforced, no dry-run pass-through.
func TestDryRun_EnabledWins(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := AuthConfig{
		Enabled:       true, // Enabled wins over DryRun
		DryRun:        true,
		StaticAPIKeys: map[string][]string{},
		DryRunMetrics: m,
	}

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// With Enabled=true and no key → 401, NOT 200.
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (Enabled wins over DryRun)", rr.Code)
	}
}

// TestDryRun_PublicEndpointNeverDenied: public health/metrics endpoints
// are always served — they must never produce a denial record even in
// dry-run mode.
func TestDryRun_PublicEndpointNeverDenied(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)
	for _, path := range []string{"/healthz", "/livez", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rr.Code)
		}
	}
	for _, v := range []string{dryRunVerdictMissingCred, dryRunVerdictInvalidKey, dryRunVerdictDeadSession} {
		if got := denialsFor(m, v); got != 0 {
			t.Fatalf("public endpoints produced %v denial records for verdict %q, want 0", got, v)
		}
	}
}

// TestDryRun_WithRealMetrics exercises the Prometheus-metric path using a
// DryRunMetrics wired via WithAuthDryRunMetrics. Confirms no panic on
// metric increment and counter increments correctly.
func TestDryRun_WithRealMetrics(t *testing.T) {
	m := NewDryRunMetrics(prometheus.NewRegistry())
	cfg := AuthConfig{
		Enabled:       false,
		DryRun:        true,
		StaticAPIKeys: map[string][]string{},
		DryRunMetrics: m,
	}

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	rr := httptest.NewRecorder()

	// Must not panic even without a DryRunRecorder.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic in dry-run metric path: %v", r)
		}
	}()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := denialsFor(m, dryRunVerdictMissingCred); got != 1 {
		t.Fatalf("missing_credential counter = %v, want 1", got)
	}
}

// TestDryRun_NilMetrics_NoPanic: DryRunMetrics not set — middleware
// must not panic.
func TestDryRun_NilMetrics_NoPanic(t *testing.T) {
	cfg := AuthConfig{
		Enabled:       false,
		DryRun:        true,
		StaticAPIKeys: map[string][]string{},
		// DryRunMetrics: nil — absent.
	}

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	rr := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with nil metrics: %v", r)
		}
	}()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

// TestDryRun_WebhookSig_NoDenial: a webhook path with the right HMAC
// signature header would pass auth — dry-run must record no denial.
func TestDryRun_WebhookSig_NoDenial(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/proj/src", nil)
	req.Header.Set("X-Vornik-Signature", "sha256=abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	for _, v := range []string{dryRunVerdictMissingCred, dryRunVerdictInvalidKey, dryRunVerdictDeadSession} {
		if got := denialsFor(m, v); got != 0 {
			t.Fatalf("webhook sig path: %v denials for verdict %q, want 0", got, v)
		}
	}
}

// TestDryRun_GenericWebhook_NoSig_NoKey_MissingCredential: dry-run +
// generic webhook path (/api/v1/webhooks/p/s) + NO signature + NO key →
// request is served (200) and verdict is "missing_credential".
//
// Pins triage semantics: when auth is enabled this path emits
// "API key or HMAC signature required" (401). In dry-run mode we report
// missing_credential so operators can distinguish "nothing at all" from
// a dead session or a bad key.
func TestDryRun_GenericWebhook_NoSig_NoKey_MissingCredential(t *testing.T) {
	m := newTestDryRunMetrics()
	cfg := buildDryRunConfig(nil, nil, m)

	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/p/s", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (dry-run must serve)", rr.Code)
	}
	if got := denialsFor(m, dryRunVerdictMissingCred); got != 1 {
		t.Fatalf("missing_credential counter = %v, want 1", got)
	}
	for _, v := range []string{dryRunVerdictInvalidKey, dryRunVerdictDeadSession} {
		if got := denialsFor(m, v); got != 0 {
			t.Fatalf("unexpected denial for verdict %q: got %v, want 0", v, got)
		}
	}
}

// TestDryRunVerdict_routeShape checks that two different path-shapes for
// the same underlying route template produce distinct dedup keys —
// which means the normaliser is working correctly.
func TestDryRunVerdict_routeShape(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/projects/p1/tasks", "/api/v1/projects/{id}/tasks"},
		{"/api/v1/projects/p2/tasks/t3", "/api/v1/projects/{id}/tasks/{id}"},
		{"/api/v1/projects/p1/tasks/t1/cancel", "/api/v1/projects/{id}/tasks/{id}/cancel"},
		{"/api/v1/projects/p1/executions", "/api/v1/projects/{id}/executions"},
		{"/api/v1/executions/ex1/pause", "/api/v1/executions/{id}/pause"},
		{"/api/v1/workflows", "/api/v1/workflows"},
		{"/healthz", "/healthz"},
	}
	for _, tc := range cases {
		got := dryRunRouteShape(tc.path)
		if got != tc.want {
			t.Errorf("dryRunRouteShape(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestDryRun_LiveSession_NoDenial: if the disabled-branch session
// resolution already stamped an identity on the context (live session),
// dry-run must record no denial — the would-be verdict is "pass".
func TestDryRun_LiveSession_NoDenial(t *testing.T) {
	m := newTestDryRunMetrics()
	sb := &stubSessionBackend{token: "live-token", projects: []string{"proj-a"}, role: "user", sessID: "s1", userID: "u1"}
	cfg := buildDryRunConfig(nil, sb, m)

	h := AuthMiddleware(cfg)(probeHandler)
	// Present a LIVE cookie — the disabled branch will resolve it and stamp
	// the identity before dry-run runs.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-a/tasks", nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "live-token"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	for _, v := range []string{dryRunVerdictMissingCred, dryRunVerdictInvalidKey, dryRunVerdictDeadSession} {
		if got := denialsFor(m, v); got != 0 {
			t.Fatalf("live session stamped — expected 0 denials for verdict %q, got %v", v, got)
		}
	}
}

// TestWithAuthDryRunMetrics_WiresMetrics: WithAuthDryRunMetrics option
// sets DryRunMetrics on the AuthConfig — pins the wiring used by
// applyMiddleware and the UI subtree.
func TestWithAuthDryRunMetrics_WiresMetrics(t *testing.T) {
	cfg := &AuthConfig{}
	m := NewDryRunMetrics(prometheus.NewRegistry())
	WithAuthDryRunMetrics(m)(cfg)
	if cfg.DryRunMetrics != m {
		t.Errorf("DryRunMetrics not set on AuthConfig via WithAuthDryRunMetrics")
	}
}

// TestWithAuthDryRunMetrics_NilIsValid: passing nil is valid (disables
// metric emission without panic).
func TestWithAuthDryRunMetrics_NilIsValid(t *testing.T) {
	cfg := &AuthConfig{}
	WithAuthDryRunMetrics(nil)(cfg)
	if cfg.DryRunMetrics != nil {
		t.Errorf("expected nil DryRunMetrics; got non-nil")
	}
}

// TestIsPublicEndpoint_UIStaticAssets — pre-flip nicety (2026-06-06
// soak triage): anonymous fetches of /ui/static/* (favicon, PWA
// manifest, htmx) showed up as would-be denials. Static assets carry
// no data; serving them publicly keeps the login screen cosmetically
// intact once auth is enforced. Path traversal is not a concern at
// THIS layer (the static file server resolves paths safely); the
// exemption is prefix-exact so /ui/staticX is NOT public.
func TestIsPublicEndpoint_UIStaticAssets(t *testing.T) {
	cases := map[string]bool{
		"/ui/static/htmx.min.js":          true,
		"/ui/static/icon.svg":             true,
		"/ui/static/manifest.webmanifest": true,
		"/ui/static/":                     true,
		"/ui/static":                      false, // bare path serves nothing
		"/ui/staticX/evil":                false,
		"/ui/tasks":                       false,
	}
	for path, want := range cases {
		if got := isPublicEndpoint(path); got != want {
			t.Errorf("isPublicEndpoint(%q) = %v, want %v", path, got, want)
		}
	}
}
