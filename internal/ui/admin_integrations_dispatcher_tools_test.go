// Tests for /ui/admin/integrations/dispatcher-tools — the
// operator-visible inventory of every dispatcher tool the LLM can
// call, plus whether its backing service is wired. Pin the
// not-wired / wired / mixed-state rendering so a future tool
// addition can't silently drop off the page.
package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubDispatcherToolInventory struct {
	rows []AdminDispatcherToolRow
}

func (s *stubDispatcherToolInventory) DispatcherTools() []AdminDispatcherToolRow {
	return s.rows
}

// TestAdminIntegrationsDispatcherTools_NotWired — without an
// inventory source the page renders the "not wired" empty state
// (route alive, source absent). Distinct copy so an operator can
// tell config-gap from empty-deployment.
func TestAdminIntegrationsDispatcherTools_NotWired(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/dispatcher-tools", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsDispatcherTools(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Dispatcher tool inventory not wired") {
		t.Errorf("missing not-wired copy; body fragment %q", firstN(body, 400))
	}
}

// TestAdminIntegrationsDispatcherTools_HappyPath — populated rows
// render with name, description, backing service, availability
// pill. Pin the wired + not-wired variants in the same render to
// catch a CSS swap that hides one state.
func TestAdminIntegrationsDispatcherTools_HappyPath(t *testing.T) {
	rows := []AdminDispatcherToolRow{
		{
			Name:           "send_email",
			Description:    "Send a fresh outbound email via the active project's email channel.",
			BackingService: "EmailSender",
			Available:      true,
		},
		{
			Name:           "memory_search",
			Description:    "Search project memory for past research.",
			BackingService: "MemorySearcher",
			Available:      false,
		},
		{
			Name:           "switch_project",
			Description:    "Set the active project for this conversation.",
			BackingService: "",
			Available:      true,
		},
	}
	srv := NewServer(WithDispatcherToolInventory(&stubDispatcherToolInventory{rows: rows}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/dispatcher-tools", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsDispatcherTools(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"send_email",
		"EmailSender",
		"memory_search",
		"MemorySearcher",
		"switch_project",
		// Availability indicators — wired + disabled paths both
		// render so a CSS swap that hides one state still fails.
		"available",
		"disabled",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body; excerpt %q", want, firstN(body, 800))
		}
	}
}

// TestAdminIntegrationsDispatcherTools_EmptyList — inventory wired
// but returning zero rows (degenerate case; should never happen in
// real deployments). Renders a distinct copy from the not-wired
// path so the operator can tell "no tools registered" apart from
// "inventory not configured."
func TestAdminIntegrationsDispatcherTools_EmptyList(t *testing.T) {
	srv := NewServer(WithDispatcherToolInventory(&stubDispatcherToolInventory{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/admin/integrations/dispatcher-tools", nil)
	rec := httptest.NewRecorder()
	srv.AdminIntegrationsDispatcherTools(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "No dispatcher tools registered") {
		t.Errorf("missing empty-list copy; body fragment %q", firstN(body, 400))
	}
}

// TestAdminRouter_IntegrationsDispatcherTools — router dispatch.
// Without this entry the route 404s even when wiring is present.
func TestAdminRouter_IntegrationsDispatcherTools(t *testing.T) {
	srv := NewServer(WithDispatcherToolInventory(&stubDispatcherToolInventory{
		rows: []AdminDispatcherToolRow{{Name: "router-probe", Available: true}},
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/integrations/dispatcher-tools", nil)
	rec := httptest.NewRecorder()
	srv.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "router-probe") {
		t.Errorf("router didn't reach handler; body %q", firstN(rec.Body.String(), 200))
	}
}
