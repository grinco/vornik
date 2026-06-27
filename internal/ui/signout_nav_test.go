// Regression tests for the global Sign out control (github-login
// phase 3 follow-up, 2026-06-05). Incident: with api.auth_enabled
// false the session was invisible to per-page data, and the Sign out
// button — previously gated ONLY on the page struct's IsSession field
// (set by exactly one of ~40 nav-bearing pages) — rendered nowhere,
// desktop or mobile. The control is now rendered on every page and
// its visibility is driven by the vornik_session_ui marker cookie via
// the signout-gate script; pages that do know the session state
// server-side render it visible without JS.
package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSignout_RenderedOnEveryNavPage — the logout forms (desktop +
// mobile tile) and the gate script must ship on a page whose data
// struct does NOT set IsSession. Hidden by default; the script
// unhides when the marker cookie is present.
func TestSignout_RenderedOnEveryNavPage(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/", nil)
	rec := httptest.NewRecorder()
	srv.AdminLanding(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if got := strings.Count(body, `data-signout `); got != 2 {
		t.Errorf("data-signout controls = %d, want 2 (desktop button + mobile tile)", got)
	}
	if got := strings.Count(body, `action="/ui/logout"`); got != 2 {
		t.Errorf("logout forms = %d, want 2", got)
	}
	// Without server-side session knowledge both controls start hidden.
	if strings.Count(body, "hidden") < 2 {
		t.Error("signout controls must default to hidden without IsSession")
	}
	if !strings.Contains(body, "vornik_session_ui=(1|admin)") {
		t.Error("signout gate script missing (marker-cookie test)")
	}
	// UI refresh 2026-06-08: mobile nav is now area-tabs + drawers (no
	// destination grid). The grid-cols widening hook was retired; the
	// gate script still unhides every [data-signout] control.
}

// TestSignout_VisibleWithServerSideSession — a page that sets
// IsSession renders the controls visible and the 7-column mobile grid
// without any JS (no-JS users still get logout where the server knows
// the state).
func TestSignout_VisibleWithServerSideSession(t *testing.T) {
	srv := NewServer()
	var buf strings.Builder
	err := srv.templates.ExecuteTemplate(&buf, "nav", map[string]any{
		"CurrentPage": "dashboard",
		"IsSession":   true,
	})
	if err != nil {
		t.Fatalf("render nav: %v", err)
	}
	body := buf.String()

	// Neither signout form may carry the hidden class when the session is
	// known server-side (desktop rail form + mobile tab-bar form).
	for _, frag := range []string{`data-signout class="hidden`, `data-signout class="hidden `} {
		if strings.Contains(body, frag) {
			t.Errorf("signout control still hidden with IsSession=true (found %q)", frag)
		}
	}
	if got := strings.Count(body, `action="/ui/logout"`); got != 2 {
		t.Errorf("logout forms = %d, want 2", got)
	}
}

// TestAdminLink_GatedOnEveryNavPage — the Admin nav item is rendered
// on every page, hidden by default, with the data-admin-link hook the
// gate script uses for the marker-cookie "admin" role hint. (Before
// 2026-06-06 it rendered ONLY when the page struct set .IsAdmin —
// which only admin pages did, so a session admin browsing /ui/tasks
// never saw the link.)
func TestAdminLink_GatedOnEveryNavPage(t *testing.T) {
	srv := NewServer()
	var buf strings.Builder
	// A page that knows nothing about the caller: link present, hidden.
	if err := srv.templates.ExecuteTemplate(&buf, "nav", map[string]any{"CurrentPage": "tasks"}); err != nil {
		t.Fatalf("render nav: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `data-admin-link`) {
		t.Fatal("Admin link not rendered — the gate script has nothing to unhide")
	}
	if !strings.Contains(body, `data-admin-link class="hidden `) {
		t.Error("Admin link must default to hidden without server-side IsAdmin")
	}
	// The gate script must recognise both marker values and the admin hook.
	for _, frag := range []string{`vornik_session_ui=(1|admin)`, `[data-admin-link]`} {
		if !strings.Contains(body, frag) {
			t.Errorf("gate script missing %q", frag)
		}
	}

	// A page that DOES know (admin pages set IsAdmin): link visible.
	buf.Reset()
	if err := srv.templates.ExecuteTemplate(&buf, "nav", map[string]any{"CurrentPage": "admin", "IsAdmin": true}); err != nil {
		t.Fatalf("render nav: %v", err)
	}
	if strings.Contains(buf.String(), `data-admin-link class="hidden `) {
		t.Error("Admin link still hidden with IsAdmin=true")
	}
}
