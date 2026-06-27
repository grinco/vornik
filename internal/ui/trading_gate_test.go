package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression (2026-06-27): the /trading dashboard was reachable on Community
// edition. The nav item's data-cap is only a client-side, fail-open hint (and an
// API-key session carries no caps cookie), and the route itself had no
// server-side gate. WithTradingEnabled — wired by the container only when the EE
// trading capability is present (c.providers.Trading != nil) — must gate the
// route: unregistered (404) on Community, registered (non-404) on Enterprise.

func tradingRouteCode(s *Server) int {
	req := httptest.NewRequest(http.MethodGet, "/trading", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestTradingRouteGate_CommunityEdition_Returns404(t *testing.T) {
	if code := tradingRouteCode(NewServer()); code != http.StatusNotFound {
		t.Errorf("community (no WithTradingEnabled): GET /trading = %d, want 404", code)
	}
}

func TestTradingRouteGate_EnterpriseEdition_NotHiddenBehind404(t *testing.T) {
	if code := tradingRouteCode(NewServer(WithTradingEnabled())); code == http.StatusNotFound {
		t.Errorf("enterprise (WithTradingEnabled): GET /trading = 404, want non-404 (route registered)")
	}
}
