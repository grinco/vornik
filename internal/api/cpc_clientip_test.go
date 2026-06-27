package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/httpx/realip"
)

// TestClientIPFromRequest_UsesContextValue — the CPC audit IP comes from
// the centrally-resolved realip context value.
func TestClientIPFromRequest_UsesContextValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(realip.WithClientIP(req.Context(), "203.0.113.7"))
	if got := clientIPFromRequest(req); got != "203.0.113.7" {
		t.Fatalf("got %q, want 203.0.113.7", got)
	}
}

// TestClientIPFromRequest_IgnoresForgedHeader is the audit-side regression
// for the Cloudflare tunnel real-IP spoof: leftmost-XFF was
// attacker-controllable. A forged X-Forwarded-For from an untrusted peer
// (no context value) must NOT land in the audit row; we key on RemoteAddr.
func TestClientIPFromRequest_IgnoresForgedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.9:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := clientIPFromRequest(req); got != "198.51.100.9" {
		t.Fatalf("got %q, want RemoteAddr host 198.51.100.9 (forged header ignored)", got)
	}
}
