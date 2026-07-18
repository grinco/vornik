package api

// Regression for CodeQL go/cookie-secure-not-set (2026-07-18). redirectToLogin
// clears the session cookie; it must carry Secure over HTTPS (direct or via a
// proxy's X-Forwarded-Proto) but NOT over plain HTTP, so the daemon's LAN HTTP
// preview keeps working.

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestIsHTTPS(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/ui/x", nil)
	if requestIsHTTPS(plain) {
		t.Error("plain HTTP request must report not-HTTPS")
	}
	tlsReq := httptest.NewRequest(http.MethodGet, "/ui/x", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if !requestIsHTTPS(tlsReq) {
		t.Error("r.TLS set must report HTTPS")
	}
	fwd := httptest.NewRequest(http.MethodGet, "/ui/x", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsHTTPS(fwd) {
		t.Error("X-Forwarded-Proto=https must report HTTPS")
	}
}

func loginCookieSecure(t *testing.T, mutate func(*http.Request)) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/dashboard", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	redirectToLogin(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "vornik_session" {
			return c.Secure
		}
	}
	t.Fatalf("vornik_session cookie not set")
	return false
}

func TestRedirectToLogin_SecureOnlyOverHTTPS(t *testing.T) {
	if loginCookieSecure(t, nil) {
		t.Error("plain HTTP: cookie must NOT be Secure")
	}
	if !loginCookieSecure(t, func(r *http.Request) { r.TLS = &tls.ConnectionState{} }) {
		t.Error("direct TLS: cookie must be Secure")
	}
	if !loginCookieSecure(t, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }) {
		t.Error("proxied HTTPS: cookie must be Secure")
	}
}
