package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/httpx/realip"
)

// next is a trivial handler that records the resolved client IP from the
// realip context, so tests can assert the middleware actually wrapped it.
func recordingNext(sink *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sink = realip.ClientIPFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestWrapRealIP_ResolvesTrustedHeader(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"10.0.0.5/32"}

	var seen string
	h, err := c.wrapRealIP(recordingNext(&seen))
	if err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:5000"
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7" {
		t.Fatalf("downstream IP: want 203.0.113.7, got %q", seen)
	}
}

func TestWrapRealIP_BadCIDRFailsAtLoad(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"not-a-cidr"}

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err == nil {
		t.Fatal("wrapRealIP: expected error for bad CIDR, got nil")
	}
}

func TestWrapRealIP_DeprecatedFallbackWarns(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	// New block empty; deprecated key set — deliberately exercising the
	// backward-compat fallback path, so the deprecation warning is expected.
	c.Config.API.RateLimit.PerIP.TrustedProxies = []string{"10.0.0.9/32"} //nolint:staticcheck // exercising the deprecated fallback on purpose

	var seen string
	h, err := c.wrapRealIP(recordingNext(&seen))
	if err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if !strings.Contains(buf.String(), "DEPRECATED") {
		t.Fatalf("expected deprecation warning, log was: %s", buf.String())
	}
	// Fallback must be functional: trusted host honours the header.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:5000"
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7" {
		t.Fatalf("deprecated fallback must honour header: got %q", seen)
	}
}

// TestWrapRealIP_AuthEnabledUnconfiguredWarns covers the startup warning
// the design requires: auth on but real_ip unconfigured is a foot-gun
// (every caller collapses to the proxy IP behind a tunnel).
func TestWrapRealIP_AuthEnabledUnconfiguredWarns(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	c.Config.API.AuthEnabled = true
	// real_ip intentionally left unconfigured.

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if !strings.Contains(buf.String(), "auth is enabled but server.real_ip is unconfigured") {
		t.Fatalf("expected auth-unconfigured warning, log was: %s", buf.String())
	}
}

func TestWrapRealIP_NoWarnWhenConfigured(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	c.Config.API.AuthEnabled = true
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"10.0.0.5/32"}

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if strings.Contains(buf.String(), "unconfigured") {
		t.Fatalf("must not warn when real_ip is configured, log was: %s", buf.String())
	}
}
