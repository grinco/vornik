package tradingauth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Backlog: "HMAC/mTLS on /internal/trading-*" (batch-2 Financials
// PRE-LIVE blockers). These tests pin the signer/verifier contract:
// a valid signature is accepted; forged, missing, expired, and
// replayed signatures are rejected. They fail before the package
// exists / when verification is a no-op, and pass once Verify is
// implemented fail-closed.

const testSecret = "super-secret-trading-key-0123456789"

func newSignedRequest(t *testing.T, signer *Signer, body string, now time.Time) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.NewReader(body))
	if err := signer.Sign(req, []byte(body), now); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return req
}

func TestSignVerifyRoundTrip(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)

	req := newSignedRequest(t, s, `{"id":"o1"}`, now)
	if err := v.Verify(req, []byte(`{"id":"o1"}`), now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestVerifyMissingHeaders(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	now := time.Unix(1_700_000_000, 0)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.NewReader("{}"))
	if err := v.Verify(req, []byte("{}"), now); err == nil {
		t.Fatal("expected rejection for missing signature headers, got nil")
	}
}

func TestVerifyForgedSignature(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{"id":"o1"}`, now)
	// Tamper with the body: signature no longer matches.
	if err := v.Verify(req, []byte(`{"id":"o1","evil":true}`), now); err == nil {
		t.Fatal("expected rejection for body tamper, got nil")
	}
	// Tamper with the signature header directly.
	req2 := newSignedRequest(t, s, `{"id":"o1"}`, now)
	req2.Header.Set(HeaderSignature, "deadbeef")
	if err := v.Verify(req2, []byte(`{"id":"o1"}`), now); err == nil {
		t.Fatal("expected rejection for forged signature, got nil")
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner("a-different-secret-entirely-9876543210")
	now := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{}`, now)
	if err := v.Verify(req, []byte(`{}`), now); err == nil {
		t.Fatal("expected rejection for wrong-secret signature, got nil")
	}
}

func TestVerifyExpiredTimestamp(t *testing.T) {
	v := NewVerifier(testSecret, 5*time.Minute)
	s := NewSigner(testSecret)
	signedAt := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{}`, signedAt)
	// Verify 6 minutes later — outside the 5-minute window.
	if err := v.Verify(req, []byte(`{}`), signedAt.Add(6*time.Minute)); err == nil {
		t.Fatal("expected rejection for expired timestamp, got nil")
	}
	// Future-skewed timestamp beyond the window is also rejected.
	if err := v.Verify(req, []byte(`{}`), signedAt.Add(-6*time.Minute)); err == nil {
		t.Fatal("expected rejection for future-skewed timestamp, got nil")
	}
}

func TestVerifyMalformedTimestamp(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{}`, now)
	req.Header.Set(HeaderTimestamp, "not-a-number")
	if err := v.Verify(req, []byte(`{}`), now); err == nil {
		t.Fatal("expected rejection for malformed timestamp, got nil")
	}
}

func TestVerifyReplayRejected(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{"id":"o1"}`, now)

	if err := v.Verify(req, []byte(`{"id":"o1"}`), now); err != nil {
		t.Fatalf("first delivery rejected: %v", err)
	}
	// Same nonce again → replay.
	if err := v.Verify(req, []byte(`{"id":"o1"}`), now); err == nil {
		t.Fatal("expected replay rejection on second use of the same nonce, got nil")
	}
}

func TestVerifyDistinctNoncesAccepted(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		req := newSignedRequest(t, s, `{}`, now)
		if err := v.Verify(req, []byte(`{}`), now); err != nil {
			t.Fatalf("delivery %d with fresh nonce rejected: %v", i, err)
		}
	}
}

func TestVerifyMethodAndPathBound(t *testing.T) {
	v := NewVerifier(testSecret, DefaultClockSkew)
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{}`, now)
	// Replay the same signed headers against a different path.
	req.URL.Path = "/api/v1/internal/trading-fills"
	if err := v.Verify(req, []byte(`{}`), now); err == nil {
		t.Fatal("expected rejection when path differs from signed path, got nil")
	}
}

func TestNonceCacheEviction(t *testing.T) {
	// Nonces older than the skew window must be evictable so the cache
	// doesn't grow without bound. After eviction the same nonce string
	// can legitimately be reused (its timestamp would be stale anyway,
	// so the freshness check is the real replay backstop past the
	// window — this test pins that the cache itself prunes).
	v := NewVerifier(testSecret, time.Minute)
	s := NewSigner(testSecret)
	base := time.Unix(1_700_000_000, 0)
	req := newSignedRequest(t, s, `{}`, base)
	if err := v.Verify(req, []byte(`{}`), base); err != nil {
		t.Fatalf("first verify failed: %v", err)
	}
	if got := v.nonceCount(); got != 1 {
		t.Fatalf("expected 1 cached nonce, got %d", got)
	}
	// A later verify (within window, fresh nonce) triggers a prune of
	// the now-stale first nonce.
	later := base.Add(2 * time.Minute)
	req2 := newSignedRequest(t, s, `{}`, later)
	if err := v.Verify(req2, []byte(`{}`), later); err != nil {
		t.Fatalf("second verify failed: %v", err)
	}
	if got := v.nonceCount(); got != 1 {
		t.Fatalf("expected stale nonce pruned (count 1), got %d", got)
	}
}

func TestSignSetsAllHeaders(t *testing.T) {
	s := NewSigner(testSecret)
	now := time.Unix(1_700_000_000, 0)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.NewReader("{}"))
	if err := s.Sign(req, []byte("{}"), now); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if req.Header.Get(HeaderSignature) == "" {
		t.Error("signature header not set")
	}
	if req.Header.Get(HeaderNonce) == "" {
		t.Error("nonce header not set")
	}
	ts := req.Header.Get(HeaderTimestamp)
	if got, _ := strconv.ParseInt(ts, 10, 64); got != now.Unix() {
		t.Errorf("timestamp header = %q, want %d", ts, now.Unix())
	}
}

func TestVerifyErrorMessage(t *testing.T) {
	e := &VerifyError{Reason: "replayed nonce"}
	if got := e.Error(); got != "trading auth: replayed nonce" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestNewVerifierDefaultsClockSkew(t *testing.T) {
	v := NewVerifier(testSecret, 0)
	if v.skew != DefaultClockSkew {
		t.Fatalf("non-positive skew should default to %v, got %v", DefaultClockSkew, v.skew)
	}
	v2 := NewVerifier(testSecret, -time.Hour)
	if v2.skew != DefaultClockSkew {
		t.Fatalf("negative skew should default to %v, got %v", DefaultClockSkew, v2.skew)
	}
}

func TestEmptySecretSignerNoop(t *testing.T) {
	// A signer with an empty secret must not stamp headers — mirrors
	// the "feature disabled" client config so a daemon with auth off
	// still accepts the unsigned request.
	s := NewSigner("")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.NewReader("{}"))
	if err := s.Sign(req, []byte("{}"), time.Now()); err != nil {
		t.Fatalf("Sign with empty secret should be a no-op, got %v", err)
	}
	if req.Header.Get(HeaderSignature) != "" {
		t.Error("empty-secret signer must not set a signature header")
	}
}
