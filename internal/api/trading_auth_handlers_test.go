package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/tradingauth"
)

// makeTradingRateLimitRegistry builds a disk-backed registry with one
// project carrying an explicit trading_rate_limit block. ordersPerMin
// of 0 leaves the trading cap unset (unlimited).
func makeTradingRateLimitRegistry(t *testing.T, ordersPerMin int) *registry.Registry {
	t.Helper()
	const projectID = "proj-a"
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte("---\nswarmId: \"s\"\nroles:\n  - name: \"r\"\n    runtime:\n      image: \"x:latest\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workflows", "w.md"), []byte("---\nworkflowId: \"w\"\nentrypoint: \"step1\"\nsteps:\n  step1:\n    type: \"agent\"\n    role: \"r\"\n    prompt: \"x\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := "projectId: \"" + projectID + "\"\ndisplayName: \"P\"\nswarmId: \"s\"\ndefaultWorkflowId: \"w\"\n"
	if ordersPerMin > 0 {
		proj += "trading_rate_limit:\n  orders_per_minute: " + itoa(ordersPerMin) + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "p.yaml"), []byte(proj), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return reg
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Backlog: "HMAC/mTLS on /internal/trading-*" + "per-project
// rate-limit before a 2nd trading project" (batch-2 Financials
// PRE-LIVE blockers). These tests pin the server-side enforcement:
// forged/missing/expired/replayed signatures → 401; valid → accepted;
// per-project trading order cap → 429 with Retry-After;
// feature-disabled path stays backward-compatible (unsigned accepted).

const handlerSecret = "trading-handler-test-secret-0123456789"

func validOrderBody() string {
	return `{"id":"o1","project_id":"proj-a","idempotency_key":"k1","symbol":"AAPL","status":"submitted","qty":1}`
}

func scopedSignedRequest(t *testing.T, signer *tradingauth.Signer, body string, now time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), projectIDKey, []string{"proj-a"}))
	if err := signer.Sign(req, []byte(body), now); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return req
}

func TestIngestTradingOrderHMACValidAccepted(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo), WithTradingAuthVerifier(v))
	now := time.Now()
	body := validOrderBody()
	req := scopedSignedRequest(t, tradingauth.NewSigner(handlerSecret), body, now)
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid signed request: got %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestIngestTradingOrderHMACMissingRejected(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo), WithTradingAuthVerifier(v))
	rec := httptest.NewRecorder()
	// No signature headers at all.
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", validOrderBody(), "proj-a"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d, want 401", rec.Code)
	}
	if repo.row != nil {
		t.Fatal("unsigned request must not persist a row when auth is enabled")
	}
}

func TestIngestTradingOrderHMACForgedRejected(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{}), WithTradingAuthVerifier(v))
	now := time.Now()
	body := validOrderBody()
	req := scopedSignedRequest(t, tradingauth.NewSigner("a-wrong-secret-value-987654321"), body, now)
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature: got %d, want 401", rec.Code)
	}
}

func TestIngestTradingOrderHMACExpiredRejected(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, time.Minute)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{}), WithTradingAuthVerifier(v))
	// Sign 10 minutes in the past relative to the verifier's clock.
	old := time.Now().Add(-10 * time.Minute)
	body := validOrderBody()
	req := scopedSignedRequest(t, tradingauth.NewSigner(handlerSecret), body, old)
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired signature: got %d, want 401", rec.Code)
	}
}

func TestIngestTradingOrderHMACReplayRejected(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{}), WithTradingAuthVerifier(v))
	now := time.Now()
	body := validOrderBody()
	signer := tradingauth.NewSigner(handlerSecret)
	req1 := scopedSignedRequest(t, signer, body, now)
	// Clone the exact signed headers for the replay (same nonce).
	req2 := scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a")
	req2.Header = req1.Header.Clone()

	rec1 := httptest.NewRecorder()
	server.IngestTradingOrder(rec1, req1)
	if rec1.Code != http.StatusNoContent {
		t.Fatalf("first delivery: got %d, want 204", rec1.Code)
	}
	rec2 := httptest.NewRecorder()
	server.IngestTradingOrder(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed delivery: got %d, want 401", rec2.Code)
	}
}

func TestIngestTradingOrderHMACDisabledBackwardCompatible(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	// No verifier wired → feature off → unsigned request still works.
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo))
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", validOrderBody(), "proj-a"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("auth-off unsigned request: got %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestIngestTradingFillHMACEnforced(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{}), WithTradingAuthVerifier(v))
	rec := httptest.NewRecorder()
	body := `{"id":"f1","order_id":"o1","project_id":"proj-a","symbol":"AAPL","qty":1}`
	server.IngestTradingFill(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", body, "proj-a"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned fill: got %d, want 401", rec.Code)
	}
}

func TestIngestTradingSafetyEventHMACEnforced(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{}), WithTradingAuthVerifier(v))
	rec := httptest.NewRecorder()
	body := `{"id":"s1","project_id":"proj-a","kind":"cap_refused"}`
	server.IngestTradingSafetyEvent(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", body, "proj-a"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned safety event: got %d, want 401", rec.Code)
	}
}

func TestGetTradingStateReplayHMACEnforced(t *testing.T) {
	v := tradingauth.NewVerifier(handlerSecret, tradingauth.DefaultClockSkew)
	server := NewServer(WithLogger(zerolog.Nop()),
		WithTradingOrderRepository(&capturingTradingOrderRepo{}),
		WithTradingFillRepository(&capturingTradingFillRepo{}),
		WithTradingAuthVerifier(v))
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=proj-a", "", "proj-a")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned state replay: got %d, want 401", rec.Code)
	}
	// Signed GET (empty body) accepted.
	signed := httptest.NewRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=proj-a", nil)
	signed = signed.WithContext(context.WithValue(signed.Context(), projectIDKey, []string{"proj-a"}))
	if err := tradingauth.NewSigner(handlerSecret).Sign(signed, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	server.GetTradingStateReplay(rec2, signed)
	if rec2.Code != http.StatusOK {
		t.Fatalf("signed state replay: got %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
}

// --- per-project trading rate limit ---

func TestIngestTradingOrderRateLimited(t *testing.T) {
	reg := makeTradingRateLimitRegistry(t, 2)
	limiter := ratelimit.New()
	server := NewServer(WithLogger(zerolog.Nop()),
		WithTradingOrderRepository(&capturingTradingOrderRepo{}),
		WithProjectRegistry(reg),
		WithTradingRateLimiter(limiter))

	post := func(idem string) int {
		body := `{"id":"` + idem + `","project_id":"proj-a","idempotency_key":"` + idem + `","symbol":"AAPL","status":"submitted","qty":1}`
		rec := httptest.NewRecorder()
		server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a"))
		return rec.Code
	}

	if c := post("a"); c != http.StatusNoContent {
		t.Fatalf("order 1: got %d, want 204", c)
	}
	if c := post("b"); c != http.StatusNoContent {
		t.Fatalf("order 2: got %d, want 204", c)
	}
	// Third within the minute → 429.
	rec := httptest.NewRecorder()
	body := `{"id":"c","project_id":"proj-a","idempotency_key":"c","symbol":"AAPL","status":"submitted","qty":1}`
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("order 3: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry a Retry-After header")
	}
}

func TestIngestTradingOrderRateLimitUnknownProject(t *testing.T) {
	// Limiter + registry wired, but the order's project isn't in the
	// registry → the gate is a no-op (no cap to apply), and the order
	// still lands.
	reg := makeTradingRateLimitRegistry(t, 2)
	server := NewServer(WithLogger(zerolog.Nop()),
		WithTradingOrderRepository(&capturingTradingOrderRepo{}),
		WithProjectRegistry(reg),
		WithTradingRateLimiter(ratelimit.New()))
	body := `{"id":"o","project_id":"proj-unknown","idempotency_key":"o","symbol":"AAPL","status":"submitted","qty":1}`
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-unknown"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unknown-project order: got %d, want 204 (no cap applies)", rec.Code)
	}
}

func TestEnforceTradingRateLimitRetryAfterRounding(t *testing.T) {
	// Direct unit test of the helper to pin the sub-second Retry-After
	// rounding branch: a per-minute cap that leaves a fractional-second
	// deficit rounds UP to the next whole second.
	reg := makeTradingRateLimitRegistry(t, 1)
	limiter := ratelimit.New()
	server := NewServer(WithLogger(zerolog.Nop()), WithProjectRegistry(reg), WithTradingRateLimiter(limiter))
	// Record one order so the next CheckKey blocks with a sub-minute
	// (fractional-second) deficit.
	limiter.RecordKey("proj-a", time.Now().Add(-30*time.Millisecond))
	rec := httptest.NewRecorder()
	if !server.enforceTradingRateLimit(rec, "proj-a") {
		t.Fatal("expected rate limit to block")
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After should round up to >= 1, got %q", ra)
	}
}

func TestIngestTradingOrderRateLimitDisabled(t *testing.T) {
	reg := makeTradingRateLimitRegistry(t, 0) // zero caps → unlimited
	limiter := ratelimit.New()
	server := NewServer(WithLogger(zerolog.Nop()),
		WithTradingOrderRepository(&capturingTradingOrderRepo{}),
		WithProjectRegistry(reg),
		WithTradingRateLimiter(limiter))
	for i := 0; i < 10; i++ {
		body := `{"id":"x","project_id":"proj-a","idempotency_key":"x","symbol":"AAPL","status":"submitted","qty":1}`
		rec := httptest.NewRecorder()
		server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a"))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("uncapped order %d: got %d, want 204", i, rec.Code)
		}
	}
}
