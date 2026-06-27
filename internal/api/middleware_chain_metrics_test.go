package api

// Tests for the auth-chain backend-verdict metric
// (vornik_auth_backend_verdicts_total{backend, verdict}), added on
// the 2026-06-07 architecture review's suggestion 8: internal/auth
// had zero metrics, so post-flip there was no way to see WHICH
// backend admits or denies traffic without grepping logs.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

func TestAuthChainMetrics_RecordsVerdicts(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewAuthChainMetrics(reg)

	dbKeyRaw, err := apikey.Generate("assistant")
	require.NoError(t, err)
	lookup := &stubAPIKeyLookup{row: &persistence.APIKey{
		ID:        "akey-m",
		ProjectID: "assistant",
		Name:      "metrics-key",
		KeyHash:   apikey.Hash(dbKeyRaw),
		KeyPrefix: apikey.DisplayPrefix(dbKeyRaw),
		CreatedAt: time.Now(),
	}}

	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"static-key-1": nil},
		APIKeyLookup:  lookup,
		ChainMetrics:  metrics,
	})
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	send := func(bearer string) int {
		req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mw(okHandler).ServeHTTP(rec, req)
		return rec.Code
	}

	// db-keys admitted.
	require.Equal(t, http.StatusOK, send(dbKeyRaw))
	// static-keys admitted: the lookup misses (hash mismatch) and the
	// chain falls through to the static map.
	lookup.row = nil
	require.Equal(t, http.StatusOK, send("static-key-1"))
	// denied: no backend admits the bogus bearer.
	require.Equal(t, http.StatusUnauthorized, send("definitely-not-a-key"))

	assert.Equal(t, 1.0,
		testutil.ToFloat64(metrics.VerdictsTotal.WithLabelValues("db-keys", "admitted")))
	assert.Equal(t, 1.0,
		testutil.ToFloat64(metrics.VerdictsTotal.WithLabelValues("static-keys", "admitted")))
	assert.Equal(t, 1.0,
		testutil.ToFloat64(metrics.VerdictsTotal.WithLabelValues("none", "denied")))
}

// Nil ChainMetrics must stay panic-free — minimal deployments and the
// container's pass-1 HTTP build (observability not yet attached) run
// the middleware without a registry, same contract as DryRunMetrics.
func TestAuthChainMetrics_NilSafe(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"k": nil},
		// ChainMetrics deliberately nil.
	})
	req := newAuthRequest(t, "/api/v1/projects/p/tasks")
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}
