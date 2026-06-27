package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/apikey"
)

// TestRequestOperatorID_APIKeyWins covers the verified-identity
// path. When the auth middleware put an api_key on the context,
// that wins over any client-supplied header — the key is the
// canonical identity, the header is a self-assertion.
func TestRequestOperatorID_APIKeyWins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Operator-Id", "victim-operator")
	ctx := context.WithValue(req.Context(), apiKeyKey, "sk-real-key")
	// Doesn't matter whether auth was enabled — key wins.
	req = req.WithContext(ctx)
	got := requestOperatorID(req)
	want := "api_key_sha256:" + apikey.Hash("sk-real-key")[:16]
	if got != want {
		t.Errorf("requestOperatorID = %q, want %q", got, want)
	}
}

func TestRequestOperatorID_DBKeyUsesRowID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), apiKeyKey, "sk-real-key")
	ctx = context.WithValue(ctx, apiKeyIDKey, "key_123")
	req = req.WithContext(ctx)
	if got := requestOperatorID(req); got != "api_key_id:key_123" {
		t.Errorf("requestOperatorID = %q, want api_key_id:key_123", got)
	}
}

// TestRequestOperatorID_HeaderRejectedWhenAuthEnabled is the
// security regression sentinel. Before the fix, a caller who
// reached a handler without an API key (e.g. on a webhook
// endpoint that accepted HMAC signature instead, or a route
// the auth middleware skipped) could send
// `X-Operator-Id: api_key:<victim>` and impersonate that
// operator's wizard sessions. The auth-enabled gate closes
// that path.
func TestRequestOperatorID_HeaderRejectedWhenAuthEnabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Operator-Id", "api_key:victim-key")
	// No apiKeyKey on context, but auth-enabled is true: the
	// header path must be refused.
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	req = req.WithContext(ctx)
	got := requestOperatorID(req)
	if got != "" {
		t.Errorf("requestOperatorID with auth enabled + header = %q, want empty (impersonation blocked)", got)
	}
}

// TestRequestOperatorID_HeaderAllowedWhenAuthDisabled covers
// the legitimate single-operator dev-mode case. With auth
// disabled the header is the only identity available, so it's
// honoured. The caveat (this is a self-asserted identity, not
// verified) is documented in the production code comment.
func TestRequestOperatorID_HeaderAllowedWhenAuthDisabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Operator-Id", "dev-operator")
	ctx := context.WithValue(req.Context(), authEnabledKey, false)
	req = req.WithContext(ctx)
	got := requestOperatorID(req)
	if got != "dev-operator" {
		t.Errorf("requestOperatorID with auth disabled + header = %q, want %q", got, "dev-operator")
	}
}

// TestRequestOperatorID_NoContextFailsClosed asserts the fail-
// closed default. If neither the auth middleware nor any test
// fixture set authEnabledKey, the header is rejected — the
// safer default. A handler test that forgets to wire the flag
// gets empty identity rather than silently honouring a header.
func TestRequestOperatorID_NoContextFailsClosed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Operator-Id", "spoofed-operator")
	// No apiKeyKey AND no authEnabledKey on context. Default
	// behaviour is "reject the header" — the auth-enabled
	// type assertion fails closed.
	got := requestOperatorID(req)
	if got != "" {
		t.Errorf("requestOperatorID with no context flag + header = %q, want empty (fail-closed)", got)
	}
}
