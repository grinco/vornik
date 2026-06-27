package api

// End-to-end pin for the dry-run counter through the FULL server stack
// (NewServer → Routes → applyMiddleware → AuthMiddleware), mirroring the
// live deployment shape: auth_enabled=false, auth_dry_run=true. Written
// during the 2026-06-06 invisible-metric incident to prove the api-side
// wiring was sound (the bug was in the service container's two-pass
// initHTTPServer — see internal/service/container_http_dryrun_metrics_test.go).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/config"
)

func TestDryRun_CounterIncrementsThroughRoutes(t *testing.T) {
	m := NewDryRunMetrics(prometheus.NewRegistry())
	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = false
	cfg.API.AuthDryRun = true
	srv := NewServer(WithConfig(cfg), WithDryRunMetrics(m))
	h := srv.Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/probe/tasks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Dry-run must SERVE the request (the handler's own 4xx for the empty
	// body is fine — what matters is auth didn't reject it)...
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("dry-run must not 401; got %d", rec.Code)
	}
	// ...and the would-be denial must land on the supplied registry.
	if got := testutil.ToFloat64(m.DenialsTotal.WithLabelValues("missing_credential")); got != 1 {
		t.Fatalf("missing_credential denials = %v, want 1 — counter not wired through Routes()", got)
	}
}
