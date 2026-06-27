package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/mcp"
)

// stubMCPRegistry returns a fixed snapshot. Lets the test exercise
// the template render without standing up a Connect/tools/list chain.
type stubMCPRegistry struct {
	snap []mcp.ServerSnapshot
}

func (s *stubMCPRegistry) Snapshot(_ context.Context) []mcp.ServerSnapshot { return s.snap }

// TestMCPIndex_RendersServerRows is the table-rendering happy path:
// reachable and unreachable servers both appear, with the right
// status badge. Locks in the page-contract the rollout brief
// requires.
func TestMCPIndex_RendersServerRows(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Minute)
	reg := &stubMCPRegistry{snap: []mcp.ServerSnapshot{
		{
			Name:          "scraper",
			Transport:     "sse",
			URL:           "http://127.0.0.1:8787",
			Reachable:     true,
			LastCheckedAt: now,
			Tools: []mcp.Tool{
				{Name: "web_fetch", Description: "Fetch a URL"},
				{Name: "ical_events"},
			},
		},
		{
			Name:          "broken",
			Transport:     "sse",
			URL:           "http://127.0.0.1:9999",
			Reachable:     false,
			Error:         "connection refused",
			LastCheckedAt: now,
		},
	}}

	srv := NewServer(WithMCPRegistry(reg))

	req := httptest.NewRequest("GET", "/mcp", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("/ui/mcp returned %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Both server names land in the table.
	if !strings.Contains(body, "scraper") {
		t.Errorf("page missing scraper row\n%s", body)
	}
	if !strings.Contains(body, "broken") {
		t.Errorf("page missing broken row")
	}

	// Status badges differentiate reachable vs unreachable.
	if !strings.Contains(body, "reachable") {
		t.Errorf("page missing reachable badge")
	}
	if !strings.Contains(body, "unreachable") {
		t.Errorf("page missing unreachable badge")
	}

	// The reachable server's tools land in the expansion panel with
	// the qualified mcp__ prefix the operator will paste into a
	// project's allowed_tools list.
	if !strings.Contains(body, "mcp__scraper__web_fetch") {
		t.Errorf("page missing qualified tool name mcp__scraper__web_fetch\n%s", body)
	}
	if !strings.Contains(body, "Fetch a URL") {
		t.Errorf("page missing tool description")
	}

	// Unreachable error surfaces so the operator can debug.
	if !strings.Contains(body, "connection refused") {
		t.Errorf("page missing unreachable error string")
	}

	// Auto-discovery ≠ auto-grant disclaimer must be on every page —
	// it's the user's headline concern.
	if !strings.Contains(body, "auto-discovery") {
		t.Errorf("page missing auto-discovery disclaimer")
	}
}

// TestMCPIndex_NoRegistry_RendersEmptyState verifies a daemon
// without an mcp.servers block renders the empty-state hint
// rather than a 500 or a confusingly blank table.
func TestMCPIndex_NoRegistry_RendersEmptyState(t *testing.T) {
	srv := NewServer()

	req := httptest.NewRequest("GET", "/mcp", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("/ui/mcp returned %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No daemon-level MCP servers configured") {
		t.Errorf("missing empty-state hint:\n%s", body)
	}
}

// TestMCPIndex_NavLink_ShownOnEveryPage spot-checks that the
// shared nav bar carries the MCP entry — without this the page
// is reachable by URL but not discoverable from the UI flow.
func TestMCPIndex_NavLink_ShownOnEveryPage(t *testing.T) {
	srv := NewServer(WithMCPRegistry(&stubMCPRegistry{}))

	req := httptest.NewRequest("GET", "/mcp", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `href="/ui/mcp"`) {
		t.Errorf("nav bar missing /ui/mcp link\n%s", body)
	}
}
