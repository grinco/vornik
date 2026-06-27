// Package api: tests for the Server.Routes shortcut + package-level
// setters that the dedicated handler tests don't trigger.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/ratelimit"
)

func TestServer_Routes_ReturnsHandler(t *testing.T) {
	srv := NewServer()
	h := srv.Routes()
	if h == nil {
		t.Fatal("Routes() returned nil")
	}
	// Drive a healthz probe through the handler to confirm it's wired.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("healthz: got %d, want 200", rec.Code)
	}
}

func TestSetDoctorHandlers(t *testing.T) {
	// Save & restore the package-level var so this test doesn't
	// leak into siblings.
	orig := doctorHandlers
	t.Cleanup(func() { doctorHandlers = orig })
	stub := &DoctorHandlers{}
	SetDoctorHandlers(stub)
	if doctorHandlers != stub {
		t.Errorf("SetDoctorHandlers didn't update the package var")
	}
}

func TestWithAuthRateLimitMetrics(t *testing.T) {
	cfg := &AuthConfig{}
	// Isolated Prometheus registry — ratelimit.NewMetrics registers
	// against the default registry by default which collides with
	// sibling tests in the package.
	m := ratelimit.NewMetrics(prometheus.NewRegistry())
	WithAuthRateLimitMetrics(m)(cfg)
	if cfg.RateLimitMetrics != m {
		t.Errorf("RateLimitMetrics not set on AuthConfig")
	}
}

func TestWithAuthRateLimitMetrics_NilIsValid(t *testing.T) {
	cfg := &AuthConfig{}
	WithAuthRateLimitMetrics(nil)(cfg)
	if cfg.RateLimitMetrics != nil {
		t.Errorf("expected nil metrics; got non-nil")
	}
}
