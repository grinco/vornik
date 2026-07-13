package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestNavPartial_AllUICallersAdminShowsAdminOnlyEntries is the
// regression test for the 2026-07-13 Community report: the Swarms,
// Workflows, and Admin/Control-plane nav entries were hidden on every
// non-admin page. The nav's two unhide signals are both EE-only — page
// data carries IsAdmin=true only on /ui/admin handlers, and the
// vornik_session_ui marker cookie is set only by the EE login flow —
// so in Community (which has no SessionBackend/login flow) they never
// fire, regardless of api.auth_enabled. WithAllUICallersAdmin() must
// render AdminOnly entries visible independent of the page's data.
func TestNavPartial_AllUICallersAdminShowsAdminOnlyEntries(t *testing.T) {
	render := func(s *Server) string {
		t.Helper()
		var buf bytes.Buffer
		if err := s.templates.ExecuteTemplate(&buf, "nav", struct {
			IsAdmin     bool
			CurrentPage string
		}{IsAdmin: false, CurrentPage: "dashboard"}); err != nil {
			t.Fatalf("ExecuteTemplate(nav): %v", err)
		}
		return buf.String()
	}

	on := render(NewServer(WithAllUICallersAdmin()))
	// Area-level: the Admin rail icon (Control plane lives in its panel).
	if !strings.Contains(on, `data-admin-link class="rail-ico`) {
		t.Errorf("nav should render the Admin rail icon visible when all callers are admin")
	}
	if strings.Contains(on, `data-admin-link class="hidden `) {
		t.Errorf("nav must not render any hidden admin-gated entry when all callers are admin")
	}
	// Destination-level: the AdminOnly panel items.
	for _, h := range []string{"/ui/swarms", "/ui/workflows", "/ui/audit"} {
		want := `href="` + h + `" data-admin-link class="panel-item`
		if !strings.Contains(on, want) {
			t.Errorf("nav: %s should render visible (want %q)", h, want)
		}
	}
	// Control plane is un-gated inside the Admin area's panel — it was
	// unreachable only because the area rail icon (asserted above) was
	// hidden. Pin that it renders as a plain panel item.
	if !strings.Contains(on, `href="/ui/admin/control-plane" class="panel-item`) {
		t.Errorf("nav: /ui/admin/control-plane should render as a plain panel item")
	}

	// Default (option unset — the EE auth-on + session-login case) keeps
	// the pre-fix contract: hidden for non-admin data. TestNavPartial_
	// AdminOnlyDests pins the details; this guards the option's default.
	off := render(NewServer())
	if !strings.Contains(off, `data-admin-link class="hidden `) {
		t.Errorf("default nav should still hide admin-gated entries for non-admin data")
	}
}
