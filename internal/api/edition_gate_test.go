package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/config"
)

// withAuthEnabled stamps auth-enabled on the request (no admin key, no scope).
func withAuthEnabled(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authEnabledKey, true))
}

// TestRequireAdminGate_CommunityEdition_501 — when the admin surface is not
// built into this edition (WithAdminConfig never wired → adminSurfacePresent
// false), an authenticated call to an admin route gets a typed 501
// EDITION_UNSUPPORTED, not a bare 404. This is what lets the CLI print
// "Enterprise-only feature" instead of "404 page not found" (papercut).
func TestRequireAdminGate_CommunityEdition_501(t *testing.T) {
	s := &Server{} // no WithAdminConfig ⇒ adminSurfacePresent=false (Community)
	rec := httptest.NewRecorder()
	ok := s.requireAdminGate(rec, withAuthEnabled(httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)))

	assert.False(t, ok, "gate must deny in Community edition")
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "want 501 EDITION_UNSUPPORTED")
	assert.Contains(t, rec.Body.String(), "EDITION_UNSUPPORTED")
}

// TestRequireAdminGate_EnterpriseAdminDisabled_404 — in Enterprise (admin
// surface present) but with admin disabled in config, the gate keeps its
// existing 404 (hide the surface) — NOT the edition 501.
func TestRequireAdminGate_EnterpriseAdminDisabled_404(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	rec := httptest.NewRecorder()
	ok := s.requireAdminGate(rec, withAuthEnabled(httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)))

	assert.False(t, ok)
	assert.Equal(t, http.StatusNotFound, rec.Code, "EE admin-disabled must stay 404, not 501")
}

// TestRequireAdminGate_AuthDisabled_Allows — single-operator (auth off)
// deployments still pass regardless of edition.
func TestRequireAdminGate_AuthDisabled_Allows(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil).
		WithContext(context.WithValue(context.Background(), authEnabledKey, false))
	assert.True(t, s.requireAdminGate(rec, req), "auth-off must allow")
}
