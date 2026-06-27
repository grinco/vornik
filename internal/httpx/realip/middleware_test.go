package realip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_PopulatesContextFromTrustedHeader(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	var seen string
	h := Middleware(c, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ClientIPFromContext(r.Context())
	}))
	r := req("10.0.0.5:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"})
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "203.0.113.7" {
		t.Fatalf("downstream context IP: want 203.0.113.7, got %q", seen)
	}
}

func TestMiddleware_OnUntrustedHeaderFiresOnlyForUntrustedSource(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	calls := 0
	mw := Middleware(c, func() { calls++ })

	// Untrusted source WITH a forwarding header → fires, keys on RemoteAddr.
	var seen string
	mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = ClientIPFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(),
		req("198.51.100.9:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"}))
	if calls != 1 {
		t.Fatalf("untrusted source with header: want 1 callback, got %d", calls)
	}
	if seen != "198.51.100.9" {
		t.Fatalf("untrusted source must key on RemoteAddr, got %q", seen)
	}

	// Trusted source with header → does NOT fire.
	mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(),
			req("10.0.0.5:1", map[string]string{"CF-Connecting-IP": "203.0.113.7"}))
	if calls != 1 {
		t.Fatalf("trusted source must not fire callback, got %d", calls)
	}

	// Untrusted source WITHOUT any header → does NOT fire.
	mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), req("198.51.100.9:1", nil))
	if calls != 1 {
		t.Fatalf("untrusted source without header must not fire callback, got %d", calls)
	}
}

func TestMiddleware_NilCallbackSafe(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	h := Middleware(c, nil)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	// Must not panic with a nil callback on an untrusted-header request.
	h.ServeHTTP(httptest.NewRecorder(),
		req("198.51.100.9:1", map[string]string{"X-Forwarded-For": "1.2.3.4"}))
}

func TestMetrics_RegisterAndIncrement(t *testing.T) {
	reg := newTestRegistry(t)
	m := NewMetrics(reg)
	if m == nil || m.UntrustedHeaderTotal == nil {
		t.Fatal("NewMetrics must register UntrustedHeaderTotal")
	}
	m.UntrustedHeaderTotal.Inc()
}
