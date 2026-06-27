package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// minimalConfigForReadyz returns a *config.Config suitable for constructing a
// Server in readyz api-layer tests. An empty Config resolves to the "all"
// profile; callers can override Node as needed.
func minimalConfigForReadyz() *config.Config {
	return &config.Config{}
}

// A relay-mode (webhook) node's readiness must reflect upstream relay
// reachability: ready when the relay check passes, 503 when it fails.
func TestReadyz_RelayCheckGatesReadiness(t *testing.T) {
	t.Run("failing check returns 503", func(t *testing.T) {
		relayErr := errors.New("relay upstream unreachable")
		srv := NewServer(
			WithReadinessCheck("relay_upstream", func(ctx context.Context) error { return relayErr }),
		)
		h := SetupRoutes(srv, minimalConfigForReadyz())

		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rw.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz must be 503 when the relay check fails, got %d", rw.Code)
		}
		body := rw.Body.String()
		if !contains(body, "relay_upstream") {
			t.Errorf("503 response body should mention the failing check name; got: %s", body)
		}
	})

	t.Run("passing check returns 200", func(t *testing.T) {
		srv := NewServer(
			WithReadinessCheck("relay_upstream", func(ctx context.Context) error { return nil }),
		)
		h := SetupRoutes(srv, minimalConfigForReadyz())

		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("readyz must be 200 when the relay check passes, got %d", rw.Code)
		}
	})
}
