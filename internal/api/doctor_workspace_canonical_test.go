package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckWorkspaceCanonical_AllClean returns OK when every
// workspace uses .autonomy/.
func TestCheckWorkspaceCanonical_AllClean(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, p, ".autonomy"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	h := &DoctorHandlers{workspacesRoot: root}
	got := h.checkWorkspaceCanonical()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK; msg=%q", got.Status, got.Message)
	}
}

// TestCheckWorkspaceCanonical_LegacyTriggersWarning returns
// WARNING with the project IDs that need migrating.
func TestCheckWorkspaceCanonical_LegacyTriggersWarning(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "stuck", "autonomy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ok", ".autonomy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := &DoctorHandlers{workspacesRoot: root}
	got := h.checkWorkspaceCanonical()
	if got.Status != "WARNING" {
		t.Errorf("status = %q, want WARNING", got.Status)
	}
	if len(got.Items) != 1 || got.Items[0] != "stuck" {
		t.Errorf("Items = %v, want [stuck]", got.Items)
	}
	if !strings.Contains(got.Message, "vornikctl workspace canonicalise") {
		t.Errorf("message should suggest the CLI fix; got %q", got.Message)
	}
}

// TestCheckWorkspaceCanonical_MixedEscalatesToError: both dirs
// present is a real bug — the canonical-context resolver flags
// the project as "mixed" source, which can silently disagree
// across machines.
func TestCheckWorkspaceCanonical_MixedEscalatesToError(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"autonomy", ".autonomy"} {
		if err := os.MkdirAll(filepath.Join(root, "double", d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	h := &DoctorHandlers{workspacesRoot: root}
	got := h.checkWorkspaceCanonical()
	if got.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR (mixed workspaces)", got.Status)
	}
	found := false
	for _, item := range got.Items {
		if strings.Contains(item, "MIXED: double") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Items should flag MIXED: double; got %v", got.Items)
	}
}

// TestCheckWorkspaceCanonical_NoRoot returns OK when the
// workspaces root isn't configured (typical for SQLite/test
// deployments that don't run containers).
func TestCheckWorkspaceCanonical_NoRoot(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkWorkspaceCanonical()
	if got.Status != "OK" {
		t.Errorf("no-root should be OK; got %q", got.Status)
	}
}
