package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// Memory firewall is a Community feature (editions matrix), so its management
// surface moved off requireAdminGate to requireOperatorScope (2026-07-03): it
// works in Community (auth-off / all-access operator) but a project-scoped
// tenant key is denied daemon-wide firewall state.
func TestMemoryFirewallMode_OperatorScoped(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()))

	// Project-scoped tenant → denied (404, no leak).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/policy/mode", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "proj-a"))
	s.AdminMemoryFirewallMode(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "tenant must not read daemon firewall mode")

	// All-access operator (auth enabled, no scope) → allowed. Works in CE
	// with no admin surface wired.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/memory/policy/mode", nil)
	req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
	s.AdminMemoryFirewallMode(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "operator must read firewall mode in any edition")
}
