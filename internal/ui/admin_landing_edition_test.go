package ui

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

// eeAdminHrefs are the admin-landing quick-links / tiles whose backing
// dependency is wired only under providers.Admin (EE) — or, for CPC,
// under providers.CrossProject. They must NOT appear on a Community
// admin console (2026-07-13: hide EE-only admin surfaces in CE, LLD
// https://docs.vornik.io).
var eeAdminHrefs = []string{
	"/ui/admin/users",
	"/ui/admin/audit",
	"/ui/admin/chat-audit",
	"/ui/admin/memory-audit",
	"/ui/admin/health/",
	"/ui/admin/health/leases",
	"/ui/admin/health/watchdog",
	"/ui/admin/health/mcp",
	"/ui/admin/integrations/email",
	"/ui/admin/integrations/dispatcher-tools",
	"/ui/admin/cpc",
	"/ui/admin/blackbox",
	"/ui/admin/blackbox/overrides",
	"/ui/admin/instincts",
}

// TestAdminLanding_CommunityHidesEESurfaces drives the real AdminLanding
// handler on a Server with NO admin dependencies wired (the Community
// shape — c.providers.Admin false leaves every adminUIOptions dep nil).
// Every EE-only quick-link/tile must be absent.
func TestAdminLanding_CommunityHidesEESurfaces(t *testing.T) {
	s := NewServer() // no admin deps → CE shape
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/admin/", nil)
	s.AdminLanding(rec, req)

	body := rec.Body.String()
	for _, href := range eeAdminHrefs {
		if strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("CE admin landing must not advertise EE surface %q", href)
		}
	}
	// The two EE-backed tiles collapse rather than showing "not wired".
	if strings.Contains(body, "Audit repository not wired") {
		t.Error("CE admin landing should hide the Recent-audit tile, not show 'not wired'")
	}
	if strings.Contains(body, "No readiness probes wired") {
		t.Error("CE admin landing should hide the Daemon-health tile, not show 'not wired'")
	}
}

// TestAdminLanding_EnterpriseShowsEESurfaces asserts the template renders
// every EE quick-link when its availability flag is set (the EE shape),
// so the gating hides only in CE and never regresses the EE console.
func TestAdminLanding_EnterpriseShowsEESurfaces(t *testing.T) {
	s := NewServer()
	data := AdminLandingData{
		adminCommonData: adminCommonData{Title: "Admin", CurrentPage: "admin", IsAdmin: true},
		// Enterprise build: the single edition switch shows every EE
		// admin surface (tiles + quick-links).
		EnterpriseAdmin: true,
		UsersWired:      true,
		AnyQuickLink:    true,
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "admin_landing.html", data); err != nil {
		t.Fatalf("ExecuteTemplate(admin_landing.html): %v", err)
	}
	body := buf.String()
	for _, href := range eeAdminHrefs {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("EE admin landing should show %q", href)
		}
	}
}
