package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
)

// Audit A5 (2026-06-10): when metrics.require_admin is set and auth is
// enabled, the main-port /metrics must require the admin key so a LAN
// scraper can't enumerate tenant labels. Default off keeps it open.

func a5Server(t *testing.T, requireAdmin, authEnabled bool) http.Handler {
	t.Helper()
	cfg := &config.Config{}
	cfg.Metrics.RequireAdmin = requireAdmin
	cfg.Metrics.ScrapeToken = "sk-scrape"
	cfg.API.AuthEnabled = authEnabled
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
	)
	return SetupRoutes(s, cfg)
}

func TestMetrics_A5_GatedRequiresAdminKey(t *testing.T) {
	h := a5Server(t, true, true)

	// No key → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Non-admin key → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-API-Key", "sk-not-admin")
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Admin key → 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-API-Key", "sk-admin")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Dedicated read-only scrape token → 200 (Prometheus path).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sk-scrape")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestMetrics_A5_OpenByDefault(t *testing.T) {
	// require_admin off → open even with auth on (back-compat).
	h := a5Server(t, false, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMetrics_A5_OpenWhenAuthDisabled(t *testing.T) {
	// require_admin on but auth off → open (single-tenant local).
	h := a5Server(t, true, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}
