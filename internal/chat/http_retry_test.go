package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestRetry_Succeeds_AfterTransient5xx verifies the happy path:
// a 500 followed by 200 produces a single successful response.
func TestRetry_Succeeds_AfterTransient5xx(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	}
	resp, err := retryableHTTPDo(context.Background(), client, buildReq, 3, 10*time.Millisecond, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("want 2 attempts, got %d", got)
	}
}

// TestRetry_GivesUp_AfterMaxAttempts — if the server never
// recovers, the last error surfaces as a *retryableHTTPError with
// the final status code.
func TestRetry_GivesUp_AfterMaxAttempts(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	}
	_, err := retryableHTTPDo(context.Background(), client, buildReq, 3, 1*time.Millisecond, zerolog.Nop())
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	rerr, ok := err.(*retryableHTTPError)
	if !ok {
		t.Fatalf("want *retryableHTTPError, got %T: %v", err, err)
	}
	if rerr.StatusCode != http.StatusBadGateway {
		t.Errorf("want 502 status in error, got %d", rerr.StatusCode)
	}
	if got := count.Load(); got != 3 {
		t.Errorf("want 3 attempts, got %d", got)
	}
}

// TestRetry_DoesNotRetry_4xx — 4xx responses (including 429) are
// returned to the caller immediately without burning retry
// attempts. 429s on our subscription surfaces are stateful and
// retrying makes them worse, not better.
func TestRetry_DoesNotRetry_4xx(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	}
	resp, err := retryableHTTPDo(context.Background(), client, buildReq, 3, 10*time.Millisecond, zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", resp.StatusCode)
	}
	if got := count.Load(); got != 1 {
		t.Errorf("429 should not retry; want 1 attempt, got %d", got)
	}
}

// TestRetry_HonorsContextCancellation — a cancelled context short-
// circuits the backoff sleep; callers that deadline out shouldn't
// wait out the remaining attempts.
func TestRetry_HonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Cancel before the second attempt's backoff completes.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	}
	start := time.Now()
	_, err := retryableHTTPDo(ctx, client, buildReq, 5, 500*time.Millisecond, zerolog.Nop())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want error from cancelled context")
	}
	// Should surface somewhere between the first attempt and
	// shortly after the cancel fires — never the full 5*500ms path.
	if elapsed > 500*time.Millisecond {
		t.Errorf("cancellation should short-circuit backoff; took %v", elapsed)
	}
}

// TestRetry_BuildReqErrorPropagates — the caller's request-building
// closure may fail (e.g. malformed URL); those errors should surface
// immediately rather than being treated as transient.
func TestRetry_BuildReqErrorPropagates(t *testing.T) {
	buildReq := func() (*http.Request, error) {
		return nil, fmt.Errorf("bad url")
	}
	_, err := retryableHTTPDo(context.Background(), &http.Client{}, buildReq, 3, 10*time.Millisecond, zerolog.Nop())
	if err == nil || !strings.Contains(err.Error(), "bad url") {
		t.Errorf("want buildReq error, got %v", err)
	}
}

// TestRetry_429_HonorsRetryAfterHeader — rate-limit hardening
// sub-item 8: when the upstream returns 429 with Retry-After: 7,
// the helper waits 7 seconds before retrying. We mock the clock
// so the test does not actually sleep — the assertion is that the
// sleepFn was invoked with ≈7s, then the second attempt succeeded.
func TestRetry_429_HonorsRetryAfterHeader(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// Mock clock: capture the durations the helper would have slept
	// on (one per retry attempt) and progress instantly.
	var sleeps []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	clock := time.Now()
	nowFn := func() time.Time { return clock }

	client := &http.Client{}
	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, srv.URL, nil)
	}
	resp, err := retryableHTTPDo(
		context.Background(), client, buildReq, 3, 10*time.Millisecond, zerolog.Nop(),
		withRetryOn429(nil),
		withRetryClock(nowFn, sleep),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 after retry, got %d", resp.StatusCode)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("want 2 attempts (429 then 200), got %d", got)
	}
	if len(sleeps) != 1 || sleeps[0] != 7*time.Second {
		t.Errorf("want exactly one sleep of 7s on retry-after, got %v", sleeps)
	}
}

// TestRetry_429_BodyHintFallback — when the response has no
// Retry-After header, the body parser callback gets to supply the
// hint (sub-item 8 step 2). The agent fall-back: a Claude-style
// JSON envelope with retry_after_seconds.
func TestRetry_429_BodyHintFallback(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n == 1 {
			// Deliberately omit Retry-After header so we exercise
			// the body parser path.
			http.Error(w, `{"error":{"retry_after_seconds":3}}`, http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	bodyParser := func(b []byte) (time.Duration, bool) {
		// Minimal inline parser for the test — production uses
		// parseClaudeRetryAfterBody.
		if strings.Contains(string(b), `"retry_after_seconds":3`) {
			return 3 * time.Second, true
		}
		return 0, false
	}

	var sleeps []time.Duration
	resp, err := retryableHTTPDo(
		context.Background(), &http.Client{},
		func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
		3, 10*time.Millisecond, zerolog.Nop(),
		withRetryOn429(bodyParser),
		withRetryClock(nil, func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if len(sleeps) != 1 || sleeps[0] != 3*time.Second {
		t.Errorf("want one 3s sleep from body hint, got %v", sleeps)
	}
}

// TestRetry_429_NoHint_SurfaceAsError — when neither header nor
// body supplies a hint, we DON'T fall back to generic exponential
// backoff for 429 (that would amplify the burst). Surface to the
// caller so it can re-queue / fail the task.
func TestRetry_429_NoHint_SurfaceAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"throttled"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := retryableHTTPDo(
		context.Background(), &http.Client{},
		func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
		3, 10*time.Millisecond, zerolog.Nop(),
		withRetryOn429(nil), // no body parser, no header → no hint
	)
	if err == nil {
		t.Fatal("want error when 429 has no retry-after hint")
	}
	rerr, ok := err.(*retryableHTTPError)
	if !ok {
		t.Fatalf("want *retryableHTTPError, got %T: %v", err, err)
	}
	if rerr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429 in error, got %d", rerr.StatusCode)
	}
}

// TestRetry_429_HintAboveCeiling_SurfacesImmediately — a server
// that says "retry in an hour" must not freeze the daemon's hot
// path. Anything > retryAfterCeiling surfaces as 429 right away so
// the agent can re-queue.
func TestRetry_429_HintAboveCeiling_SurfacesImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600") // 1 hour
		http.Error(w, "wait", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var sleeps []time.Duration
	_, err := retryableHTTPDo(
		context.Background(), &http.Client{},
		func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
		3, 10*time.Millisecond, zerolog.Nop(),
		withRetryOn429(nil),
		withRetryClock(nil, func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		}),
	)
	if err == nil {
		t.Fatal("want error when retry-after exceeds ceiling")
	}
	rerr, ok := err.(*retryableHTTPError)
	if !ok || rerr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want *retryableHTTPError(429), got %T %v", err, err)
	}
	if rerr.RetryAfter != 3600*time.Second {
		t.Errorf("want retry-after 3600s on error for caller diagnostics, got %v", rerr.RetryAfter)
	}
	if len(sleeps) != 0 {
		t.Errorf("must NOT sleep when retry-after exceeds ceiling; got %v", sleeps)
	}
}

// TestRetry_429_DefaultBehaviorUnchanged — without withRetryOn429
// opt-in, the helper preserves its legacy behaviour: 429 passes
// through to the caller as-is. This protects existing call sites
// (Bedrock, codex) that haven't migrated.
func TestRetry_429_DefaultBehaviorUnchanged(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "throttled", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	resp, err := retryableHTTPDo(
		context.Background(), &http.Client{},
		func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) },
		3, 10*time.Millisecond, zerolog.Nop(),
		// no withRetryOn429 — legacy mode.
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("want 429 (legacy passthrough), got %d", resp.StatusCode)
	}
	if got := count.Load(); got != 1 {
		t.Errorf("legacy mode must not retry 429; got %d attempts", got)
	}
}

// TestParseRetryAfterHeader covers each shape of the RFC 7231 §7.1.3
// header: numeric seconds, HTTP-date, malformed, negative.
func TestParseRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		hdr  string
		want time.Duration
	}{
		{"empty", "", 0},
		{"numeric seconds", "5", 5 * time.Second},
		{"with whitespace", "  10  ", 10 * time.Second},
		{"zero is no-hint", "0", 0},
		{"negative is no-hint", "-3", 0},
		{"RFC1123 future", "Sun, 17 May 2026 12:00:30 GMT", 30 * time.Second},
		{"RFC1123 past", "Sun, 17 May 2026 11:59:30 GMT", 0},
		{"garbage", "blarg", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRetryAfterHeader(c.hdr, now)
			if got != c.want {
				t.Errorf("parseRetryAfterHeader(%q) = %v, want %v", c.hdr, got, c.want)
			}
		})
	}
}

// TestPickRetryAfter — header precedence over body when both are
// present; body fallback when header is absent.
func TestPickRetryAfter(t *testing.T) {
	bodyParser := func(b []byte) (time.Duration, bool) {
		if string(b) == "yes" {
			return 9 * time.Second, true
		}
		return 0, false
	}

	d, ok := pickRetryAfter(3*time.Second, []byte("yes"), bodyParser)
	if !ok || d != 3*time.Second {
		t.Errorf("header should win over body: got (%v, %v)", d, ok)
	}

	d, ok = pickRetryAfter(0, []byte("yes"), bodyParser)
	if !ok || d != 9*time.Second {
		t.Errorf("body fallback failed: got (%v, %v)", d, ok)
	}

	d, ok = pickRetryAfter(0, []byte("no"), bodyParser)
	if ok || d != 0 {
		t.Errorf("no hint should report ok=false: got (%v, %v)", d, ok)
	}

	d, ok = pickRetryAfter(0, nil, nil)
	if ok || d != 0 {
		t.Errorf("no parser, no header → no hint: got (%v, %v)", d, ok)
	}
}

// TestRetryableHTTPError_FormatIncludesRetryAfter — the error's
// Error() must include the retry-after window when present so the
// caller's chained "claude subscription: %w" surfaces useful info
// to the operator.
func TestRetryableHTTPError_FormatIncludesRetryAfter(t *testing.T) {
	e := &retryableHTTPError{StatusCode: 429, Body: "throttled", RetryAfter: 7 * time.Second}
	if !strings.Contains(e.Error(), "7s") {
		t.Errorf("error string must mention retry-after: %s", e.Error())
	}
	if !strings.Contains(e.Error(), "429") {
		t.Errorf("error string must mention status code: %s", e.Error())
	}

	// Without retry-after, format stays grep-stable.
	e2 := &retryableHTTPError{StatusCode: 500, Body: "boom"}
	if e2.Error() != "HTTP 500: boom" {
		t.Errorf("legacy format must stay stable: %s", e2.Error())
	}
}

// TestRetry_GenericBackoffOn429_WithoutRetryAfter: a 429 that carries
// NO Retry-After header (e.g. Vertex RESOURCE_EXHAUSTED) must retry on
// generic backoff when withGenericBackoffOn429 is set, then succeed.
// Without that option the hint-only path would surface immediately.
func TestRetry_GenericBackoffOn429_WithoutRetryAfter(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			// No Retry-After header — the Vertex shape.
			http.Error(w, `{"error":{"code":429,"message":"Resource exhausted"}}`, http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) }
	resp, err := retryableHTTPDo(context.Background(), client, buildReq, 3, 5*time.Millisecond, zerolog.Nop(),
		withRetryOn429(nil), withGenericBackoffOn429())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 after 429 backoff retry, got %d", resp.StatusCode)
	}
	if got := count.Load(); got != 2 {
		t.Errorf("want 2 attempts, got %d", got)
	}
}

// TestRetry_429WithoutBackoffOption_SurfacesImmediately: the default
// (no withGenericBackoffOn429) keeps the conservative behaviour — a
// 429 without a Retry-After hint surfaces at once, no retry.
func TestRetry_429WithoutBackoffOption_SurfacesImmediately(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Error(w, `{"error":"rate"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &http.Client{}
	buildReq := func() (*http.Request, error) { return http.NewRequest(http.MethodGet, srv.URL, nil) }
	_, err := retryableHTTPDo(context.Background(), client, buildReq, 3, 5*time.Millisecond, zerolog.Nop(),
		withRetryOn429(nil))
	if err == nil {
		t.Fatal("expected a retryableHTTPError for an un-hinted 429")
	}
	if got := count.Load(); got != 1 {
		t.Errorf("hint-only 429 must not retry; want 1 attempt, got %d", got)
	}
}
