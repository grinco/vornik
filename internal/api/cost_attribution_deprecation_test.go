package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// TestMaybeWarnLegacyHeaderShadowed_StampsDeprecationHeader pin
// the user-visible side-effect: when a client presents BOTH a
// DB-backed key (via projectIDFromKeyKey context) AND the
// legacy X-Vornik-Project-ID header, the response carries
// `Deprecation: true` so the client can branch on it without
// log scraping.
func TestMaybeWarnLegacyHeaderShadowed_StampsDeprecationHeader(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	req.Header.Set("X-Vornik-Project-ID", "header-project")
	req = req.WithContext(context.WithValue(req.Context(), projectIDFromKeyKey, "key-project"))
	rr := httptest.NewRecorder()

	s.maybeWarnLegacyHeaderShadowed(rr, req)

	if got := rr.Header().Get("Deprecation"); got != "true" {
		t.Errorf("Deprecation header = %q, want true", got)
	}
}

// TestMaybeWarnLegacyHeaderShadowed_SkippedWithoutBoth: the
// header is set only when BOTH signals are present; a
// header-without-key or key-without-header doesn't trip it.
func TestMaybeWarnLegacyHeaderShadowed_SkippedWithoutBoth(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}

	// header only, no DB-backed key
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("X-Vornik-Project-ID", "header-only")
	rr := httptest.NewRecorder()
	s.maybeWarnLegacyHeaderShadowed(rr, req)
	if got := rr.Header().Get("Deprecation"); got != "" {
		t.Errorf("header-only: Deprecation should NOT be set; got %q", got)
	}

	// DB-key only, no header
	req2 := httptest.NewRequest(http.MethodPost, "/x", nil)
	req2 = req2.WithContext(context.WithValue(req2.Context(), projectIDFromKeyKey, "key-project"))
	rr2 := httptest.NewRecorder()
	s.maybeWarnLegacyHeaderShadowed(rr2, req2)
	if got := rr2.Header().Get("Deprecation"); got != "" {
		t.Errorf("key-only: Deprecation should NOT be set; got %q", got)
	}
}

// TestMaybeWarnLegacyHeaderShadowed_RateLimitsLog verifies the
// log warn doesn't spam — same client retrying within 5
// minutes only logs once. Response header is stamped every
// time regardless (the header is the canonical signal).
func TestMaybeWarnLegacyHeaderShadowed_RateLimitsLog(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}

	mkReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
		req.Header.Set("X-Vornik-Project-ID", "header-project")
		req.Header.Set("User-Agent", "test-client/1.0")
		return req.WithContext(context.WithValue(req.Context(), projectIDFromKeyKey, "key-project"))
	}

	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		s.maybeWarnLegacyHeaderShadowed(rr, mkReq())
		if got := rr.Header().Get("Deprecation"); got != "true" {
			t.Errorf("iter %d: Deprecation = %q, want true", i, got)
		}
	}
	// The warner's lastFor map should have exactly one entry for
	// our (path, UA) pair — proves the rate-limit logic
	// recognised the same client across calls.
	if s.legacyHeaderShadowedWarner == nil {
		t.Fatalf("warner should have been initialised")
	}
	s.legacyHeaderShadowedWarner.mu.Lock()
	got := len(s.legacyHeaderShadowedWarner.lastFor)
	s.legacyHeaderShadowedWarner.mu.Unlock()
	if got != 1 {
		t.Errorf("lastFor should have 1 entry; got %d (rate-limit not collapsing same client)", got)
	}
}

// TestMaybeWarnLegacyHeaderShadowed_NilSafety: nil request or
// nil responseWriter should not crash. Defensive — the helper
// is called from the chat-proxy hot path where a degraded
// request shouldn't blow up the daemon.
func TestMaybeWarnLegacyHeaderShadowed_NilSafety(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	s.maybeWarnLegacyHeaderShadowed(nil, nil)
	s.maybeWarnLegacyHeaderShadowed(httptest.NewRecorder(), nil)
}
