package service

// End-to-end regression test for the Community nav wiring.
//
// Incident (2026-07-13, Community report): the Swarms/Workflows/Admin
// (Control plane) nav entries were hidden on every non-admin page in a
// Community deployment even though every one of those surfaces was
// reachable. The nav's two unhide signals are both EE-only — the
// /ui/admin handlers' IsAdmin data bit, and the EE login flow's
// vornik_session_ui marker cookie — and Community has neither (its
// identity/SessionBackend provider is nil). A first fix keyed on
// api.auth_enabled=false, but CE defaults auth ON, so it never fired.
//
// The corrected wiring shows the entries whenever no non-admin browser
// session can exist: sessionLogin == nil (always true in CE, since
// providers.Identity is nil in a non-enterprise build) OR auth off.
// This test drives the REAL NewContainer boot path with auth ON to pin
// the sessionLogin==nil path specifically — the exact CE scenario —
// so dropping the wiring line fails a real test (same recipe as
// container_http_fixit_ui_wiring_test.go).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewContainer_CommunityRendersAdminNavEntries(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	// Auth ON — the CE default. Proves the fix does NOT depend on
	// auth-off; it fires because a non-enterprise build has no identity
	// provider, so buildSessionLogin() returns nil.
	cfg.API.AuthEnabled = true
	// A static bearer key so the request clears the auth chain (CE
	// auth-on browser access is API-key based — there is no session
	// login). The key's admin-ness is irrelevant to the nav: the fix
	// overrides hasAdminFlag regardless; the key only gets past auth.
	const key = "sk-vornik-ce-nav-wiring-test"
	cfg.API.APIKeys = []string{key}

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.providers.Identity != nil {
		t.Fatal("precondition: a non-enterprise build must have no identity provider (sessionLogin == nil)")
	}

	// Auth is on, so present the static key. A static (unscoped) key is
	// all-access, so /ui/tasks does not project-scope 403. Assert the nav.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/tasks: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-admin-link class="rail-ico`) {
		t.Error("CE /ui/tasks: Admin rail icon should render visible — ui.WithAllUICallersAdmin is not wired in the uiOpts site")
	}
	for _, h := range []string{"/ui/swarms", "/ui/workflows"} {
		if !strings.Contains(body, `href="`+h+`" data-admin-link class="panel-item`) {
			t.Errorf("CE /ui/tasks: %s nav entry should render visible", h)
		}
	}
}
