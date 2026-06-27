package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestRunMCPServers_RendersTable hits the stubbed /api/v1/mcp/servers
// endpoint and asserts the CLI produces a tabbed table with the
// expected columns. This is the operator-facing surface — drift here
// is hard to catch without a dedicated test because no other path
// shells out to runMCPServers.
func TestRunMCPServers_RendersTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp/servers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"servers": []map[string]any{
				{
					"name":            "scraper",
					"transport":       "sse",
					"url":             "http://127.0.0.1:8787",
					"reachable":       true,
					"tools":           []map[string]string{{"name": "web_fetch"}, {"name": "ical_events"}},
					"last_checked_at": "2026-05-18T10:00:00Z",
				},
				{
					"name":            "broken",
					"transport":       "sse",
					"url":             "http://127.0.0.1:9999",
					"reachable":       false,
					"error":           "connection refused",
					"tools":           nil,
					"last_checked_at": "2026-05-18T10:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test")

	// Capture stdout so we can assert on the rendered table without
	// depending on cobra's output piping (mcpServersCmd writes to
	// os.Stdout directly via tabwriter).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Reset the --json flag in case a previous test in the same
	// binary toggled it.
	mcpJSON = false
	runErr := runMCPServers(mcpServersCmd, nil)

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = old

	if runErr != nil {
		t.Fatalf("runMCPServers: %v", runErr)
	}

	got := buf.String()
	if !strings.Contains(got, "SERVER") || !strings.Contains(got, "TRANSPORT") {
		t.Errorf("table headers missing\n%s", got)
	}
	if !strings.Contains(got, "scraper") {
		t.Errorf("scraper row missing\n%s", got)
	}
	if !strings.Contains(got, "broken") {
		t.Errorf("broken row missing\n%s", got)
	}
	if !strings.Contains(got, "reachable") {
		t.Errorf("reachable status missing\n%s", got)
	}
	if !strings.Contains(got, "unreachable") {
		t.Errorf("unreachable status missing\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("error string missing\n%s", got)
	}
	if !strings.Contains(got, "Total: 2") {
		t.Errorf("total summary missing\n%s", got)
	}
}

// TestRunMCPServers_JSONPassthrough exercises the --json branch:
// when the operator wants raw JSON, the CLI pretty-prints the body
// the daemon returned untouched. The table-rendering branch is
// the default, so we explicitly flip mcpJSON for this test.
func TestRunMCPServers_JSONPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers":[{"name":"scraper","transport":"sse","reachable":true,"tools":[]}]}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	mcpJSON = true
	defer func() { mcpJSON = false }()
	runErr := runMCPServers(mcpServersCmd, nil)

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = old

	if runErr != nil {
		t.Fatalf("runMCPServers --json: %v", runErr)
	}
	got := buf.String()
	if !strings.Contains(got, `"name": "scraper"`) {
		t.Errorf("JSON output missing scraper:\n%s", got)
	}
}

// TestRunMCPServers_EmptyCatalog confirms the no-config case prints
// "Total: 0" rather than crashing on an empty servers array.
func TestRunMCPServers_EmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers":[]}`))
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	mcpJSON = false
	runErr := runMCPServers(mcpServersCmd, nil)
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = old

	if runErr != nil {
		t.Fatalf("runMCPServers: %v", runErr)
	}
	if !strings.Contains(buf.String(), "Total: 0") {
		t.Errorf("expected Total: 0 in empty-catalog output, got:\n%s", buf.String())
	}
}
