package ui

// Regression for CodeQL go/cookie-secure-not-set (2026-07-18). The chat session
// cookie must carry Secure over HTTPS (direct or via a TLS-terminating proxy's
// X-Forwarded-Proto) but NOT over plain HTTP — the daemon's LAN preview serves
// plain HTTP, where a Secure cookie would silently never be stored.

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func chatCookieSecureFor(t *testing.T, mutate func(*http.Request)) bool {
	t.Helper()
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/chat", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	data := &ChatPageData{}
	s.ensureChatCookie(rec, req, data)
	for _, c := range rec.Result().Cookies() {
		if c.Name == chatSessionCookie {
			return c.Secure
		}
	}
	t.Fatalf("chat session cookie %q not set", chatSessionCookie)
	return false
}

func TestEnsureChatCookie_SecureOnlyOverHTTPS(t *testing.T) {
	if chatCookieSecureFor(t, nil) {
		t.Error("plain HTTP: cookie must NOT be Secure (would break the LAN HTTP preview)")
	}
	if !chatCookieSecureFor(t, func(r *http.Request) { r.TLS = &tls.ConnectionState{} }) {
		t.Error("direct TLS: cookie must be Secure")
	}
	if !chatCookieSecureFor(t, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }) {
		t.Error("proxied HTTPS (X-Forwarded-Proto): cookie must be Secure")
	}
}
