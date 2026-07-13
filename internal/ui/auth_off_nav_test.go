package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestNavPartial_AuthDisabledShowsAdminOnlyEntries is the regression
// test for the 2026-07-13 Community report: with API auth disabled
// (the CE default) the Swarms, Workflows, and Admin/Control-plane nav
// entries were hidden on every non-admin page, even though every one
// of those surfaces is reachable (admin.Middleware stamps IsAdmin for
// all callers when auth is off, and the admin gate disengages). The
// two signals the nav relies on are both absent in that mode: page
// data only carries IsAdmin=true on /ui/admin handlers, and the
// vornik_session_ui marker cookie is only set by the EE login flow.
// WithAuthDisabled() must render AdminOnly entries visible regardless
// of the page's data.
func TestNavPartial_AuthDisabledShowsAdminOnlyEntries(t *testing.T) {
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

	authOff := render(NewServer(WithAuthDisabled()))
	// Area-level: the Admin rail icon (Control plane lives in its panel).
	if !strings.Contains(authOff, `data-admin-link class="rail-ico`) {
		t.Errorf("auth-off nav should render the Admin rail icon visible")
	}
	if strings.Contains(authOff, `data-admin-link class="hidden `) {
		t.Errorf("auth-off nav must not render any hidden admin-gated entry")
	}
	// Destination-level: the AdminOnly panel items.
	for _, h := range []string{"/ui/swarms", "/ui/workflows", "/ui/audit"} {
		want := `href="` + h + `" data-admin-link class="panel-item`
		if !strings.Contains(authOff, want) {
			t.Errorf("auth-off nav: %s should render visible (want %q)", h, want)
		}
	}
	// Control plane is un-gated inside the Admin area's panel — it was
	// unreachable only because the area rail icon (asserted above) was
	// hidden. Pin that it renders as a plain panel item.
	if !strings.Contains(authOff, `href="/ui/admin/control-plane" class="panel-item`) {
		t.Errorf("auth-off nav: /ui/admin/control-plane should render as a plain panel item")
	}

	// Default (auth on) keeps the pre-fix contract: hidden for non-admin
	// data — TestNavPartial_AdminOnlyDests pins the details; this guards
	// the option's default wiring.
	authOn := render(NewServer())
	if !strings.Contains(authOn, `data-admin-link class="hidden `) {
		t.Errorf("default nav should still hide admin-gated entries for non-admin data")
	}
}
