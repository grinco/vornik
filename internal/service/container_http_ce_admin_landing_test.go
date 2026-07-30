package service

// End-to-end regression test: the Community admin landing must not
// advertise EE-only surfaces (2026-07-13; LLD https://docs.vornik.io
// 2026-07-13-ce-admin-console-ee-gating-design.md). A non-enterprise
// NewContainer leaves providers.Admin (and providers.CrossProject)
// false, so every EE admin dep is nil; the landing gates each
// quick-link/tile on its dep and omits the EE ones. Drives the real
// boot path (same recipe as container_http_ce_nav_wiring_test.go) so a
// regression that un-gates a link fails here.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewContainer_CommunityAdminLandingHidesEESurfaces(t *testing.T) {
	// Auth off — the admin gate (admin.Middleware) 404s /ui/admin/* when
	// admin config is disabled, and WithAdminConfig is itself wired only
	// under providers.Admin (nil in CE). Auth-off disengages that gate so
	// the landing renders; the landing's EE dep-gating (what's under test)
	// is independent of auth. providers.Admin stays false (edition), so
	// every EE admin dep is nil regardless.
	cfg := newComposerWiringTestConfig(t) // AuthEnabled defaults false

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.providers.Admin {
		t.Fatal("precondition: a non-enterprise build must have providers.Admin=false")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/", nil)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/admin/: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// EE surfaces whose backing dep is wired only under providers.Admin
	// (or providers.CrossProject) — must be absent in CE.
	for _, href := range []string{
		"/ui/admin/users", "/ui/admin/audit", "/ui/admin/chat-audit",
		"/ui/admin/memory-audit", "/ui/admin/health/", "/ui/admin/health/leases",
		"/ui/admin/health/watchdog", "/ui/admin/health/mcp",
		"/ui/admin/integrations/email", "/ui/admin/integrations/dispatcher-tools",
		"/ui/admin/cpc", "/ui/admin/blackbox", "/ui/admin/instincts",
	} {
		if strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("CE admin landing must not advertise EE surface %q", href)
		}
	}
	// And it must not show the EE "not wired" placeholders.
	if strings.Contains(body, "not wired") {
		t.Error("CE admin landing should hide EE tiles, not render 'not wired' placeholders")
	}
}
