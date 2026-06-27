package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAdminAudit_TableOutput runs the CLI against a stub server
// that returns one canned entry, then captures stdout and checks
// the rendered table.
func TestAdminAudit_TableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/audit" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("action"); got != "mcp.refresh" {
			t.Errorf("action filter not forwarded: got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adminAuditResponse{
			Entries: []adminAuditRow{
				{
					ID: "admaud-1", Timestamp: "2026-05-18T14:00:00Z",
					Principal: "sk-admin", Source: "ui",
					Action: "mcp.refresh", Target: "proj-a", IP: "10.0.0.1",
				},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	adminAuditAction = "mcp.refresh"
	adminAuditPrincipal = ""
	adminAuditTarget = ""
	adminAuditLimit = 10
	adminAuditJSON = false

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	if err := runAdminAudit(adminAuditCmd, nil); err != nil {
		_ = w.Close()
		t.Fatalf("runAdminAudit: %v", err)
	}
	_ = w.Close()

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "mcp.refresh") {
		t.Errorf("missing action row in table: %s", out)
	}
	if !strings.Contains(out, "sk-admin") {
		t.Errorf("missing principal in table: %s", out)
	}
}

// TestAdminAudit_JSONOutput exercises the --json branch.
func TestAdminAudit_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adminAuditResponse{
			Entries: []adminAuditRow{
				{ID: "admaud-2", Timestamp: "2026-05-18T14:00:00Z", Action: "config.reload"},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	adminAuditAction = ""
	adminAuditPrincipal = ""
	adminAuditTarget = ""
	adminAuditLimit = 10
	adminAuditJSON = true
	defer func() { adminAuditJSON = false }()

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	if err := runAdminAudit(adminAuditCmd, nil); err != nil {
		_ = w.Close()
		t.Fatalf("runAdminAudit: %v", err)
	}
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, `"id": "admaud-2"`) {
		t.Errorf("missing JSON field: %s", out)
	}
}

// TestAdminAudit_404Propagates — the daemon's "disabled" 404 must
// reach the CLI as an APIError.
func TestAdminAudit_404Propagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"admin disabled"}}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "sk-admin")
	adminAuditJSON = false
	adminAuditAction = ""
	adminAuditPrincipal = ""
	adminAuditTarget = ""
	adminAuditLimit = 10
	err := runAdminAudit(adminAuditCmd, nil)
	if err == nil {
		t.Fatal("expected 404 to propagate as error")
	}
}

// TestTruncate covers the small helper.
func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate(\"abcdef\", 4) = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate(\"abc\", 10) = %q (should pass through)", got)
	}
}
