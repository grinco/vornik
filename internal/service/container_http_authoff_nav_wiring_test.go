package service

// End-to-end regression test for the auth-off nav wiring.
//
// Incident (2026-07-13, Community report): with api.auth_enabled=false
// (the CE default) the Swarms/Workflows/Admin (Control plane) nav
// entries were hidden on every non-admin page even though every one of
// those surfaces was reachable — admin.Middleware stamps IsAdmin for
// all callers when auth is off, but neither of the nav's unhide
// signals (the /ui/admin handlers' IsAdmin data bit; the EE login
// flow's vornik_session_ui marker cookie) exists on CE non-admin
// pages. The fix wires ui.WithAuthDisabled at the uiOpts site; this
// test drives the REAL NewContainer boot path so dropping that line
// fails a real test (same recipe as container_http_fixit_ui_wiring_test.go).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewContainer_AuthOffRendersAdminNavEntries(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	if cfg.API.AuthEnabled {
		t.Fatal("precondition: test recipe must have auth disabled")
	}

	c, err := NewContainer(cfg, "")
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks", nil)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/tasks: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-admin-link class="rail-ico`) {
		t.Error("auth-off /ui/tasks: Admin rail icon should render visible — ui.WithAuthDisabled is not wired in the uiOpts site")
	}
	for _, h := range []string{"/ui/swarms", "/ui/workflows"} {
		if !strings.Contains(body, `href="`+h+`" data-admin-link class="panel-item`) {
			t.Errorf("auth-off /ui/tasks: %s nav entry should render visible", h)
		}
	}
}
