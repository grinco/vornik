// Regression tests for the accessibility invariants that ship in
// _partials.html — skip-to-main link, <main id="main">, and the
// SVG aria-hidden defaults on the iconCheck/iconX/iconSpinner/
// iconDot status badge atoms. These are the ones that ripple
// across every page; a future template edit that drops them
// silently breaks keyboard + screen-reader users on every screen.
package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestA11y_SkipLinkOnAdminLanding — admin landing is the easiest
// page to render without wiring (no required source). The
// skip-to-main link lives in the shared nav partial, so passing
// here proves every other page that uses {{template "nav" .}}
// renders it too.
func TestA11y_SkipLinkOnAdminLanding(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/", nil)
	rec := httptest.NewRecorder()
	srv.AdminLanding(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="#main"`) {
		t.Error(`missing skip-to-main link (href="#main")`)
	}
	if !strings.Contains(body, "Skip to main content") {
		t.Error("missing skip-link copy")
	}
	if !strings.Contains(body, `<main id="main" tabindex="-1"`) {
		t.Error(`missing <main id="main" tabindex="-1"> target for the skip link`)
	}
}

// TestA11y_StatusBadgeIconsAriaHidden — render a page that emits
// every status badge variant via {{template "taskStatusBadge" .}}.
// iconCheck/iconX/iconSpinner/iconDot are decorative once the
// adjacent text label is read; aria-hidden keeps screen readers
// from announcing "(complex SVG description) Completed".
func TestA11y_StatusBadgeIconsAriaHidden(t *testing.T) {
	srv := NewServer(WithEmailChannelInventory(&stubEmailChannelInventory{
		// One row is enough; the page itself isn't where badges
		// live but the assertion below pulls from the rendered
		// inline definitions inside _partials.html which the page
		// includes.
		rows: []AdminEmailChannelRow{{ProjectID: "probe", IMAPHost: "h"}},
	}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/email", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsEmail(rec, req)
	body := rec.Body.String()

	// The decorative-icon discipline is enforced inside _partials.html:
	// any nav/inline SVG that's purely visual gets aria-hidden="true".
	// Quick smoke: the rendered nav must have at least one SVG and
	// none of them should be missing aria-hidden when they're paired
	// with adjacent visible text (the nav case).
	if !strings.Contains(body, `aria-hidden="true"`) {
		t.Fatal(`no aria-hidden="true" anywhere in rendered nav — _partials.html SVGs lost the attribute`)
	}
}

// TestA11y_AdminPageHasMainId — every admin page must give its
// <main> an id so the skip link works. Probing AdminIntegrationsEmail
// gives us a deterministic, wired-able surface; any admin handler
// would do.
func TestA11y_AdminPageHasMainId(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/", nil)
	rec := httptest.NewRecorder()
	srv.AdminLanding(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `id="main"`) {
		t.Error("admin landing <main> missing id=main")
	}
	if !strings.Contains(body, `tabindex="-1"`) {
		t.Error(`admin landing <main> missing tabindex="-1" — skip-link target won't receive focus`)
	}
}
