package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestWorkflowShapeIssue_MissingDescription — the pure helper
// flags an empty description with a clear instruction; this is
// the hot path for the doctor report items list.
func TestWorkflowShapeIssue_MissingDescription(t *testing.T) {
	wf := &registry.Workflow{ID: "x", Entrypoint: "plan"}
	got := workflowShapeIssue(wf)
	if got == "" {
		t.Fatal("expected issue string for missing description")
	}
	if !strings.Contains(got, "description") {
		t.Errorf("issue = %q, want mention of description", got)
	}
}

// TestWorkflowShapeIssue_WhitespaceOnly — a description that's only
// whitespace counts as missing. Operators sometimes type a space
// to "satisfy" a required-field lint; we don't let that pass.
func TestWorkflowShapeIssue_WhitespaceOnly(t *testing.T) {
	wf := &registry.Workflow{ID: "x", Description: "   \t\n"}
	if workflowShapeIssue(wf) == "" {
		t.Error("whitespace-only description must be flagged")
	}
}

// TestWorkflowShapeIssue_OverLimit — descriptions past the cap are
// flagged even though Validate would have rejected them at load.
// Defence-in-depth: the doctor catches anything that somehow
// bypassed the loader (e.g. a hot-reload race).
func TestWorkflowShapeIssue_OverLimit(t *testing.T) {
	wf := &registry.Workflow{
		ID:          "x",
		Description: strings.Repeat("a", registry.WorkflowDescriptionMaxLen+5),
	}
	got := workflowShapeIssue(wf)
	if got == "" {
		t.Fatal("expected issue for over-limit description")
	}
	if !strings.Contains(got, "cap") {
		t.Errorf("issue = %q, want mention of the cap", got)
	}
}

// TestWorkflowShapeIssue_Healthy — a workflow with a sane
// description returns the empty string (no issue).
func TestWorkflowShapeIssue_Healthy(t *testing.T) {
	wf := &registry.Workflow{
		ID:          "x",
		Description: "A perfectly reasonable summary.",
	}
	if got := workflowShapeIssue(wf); got != "" {
		t.Errorf("healthy workflow flagged: %q", got)
	}
}

// TestWorkflowShapeIssue_Nil — defensive: nil input doesn't panic.
func TestWorkflowShapeIssue_Nil(t *testing.T) {
	if workflowShapeIssue(nil) == "" {
		t.Error("nil workflow should yield a non-empty issue, not pass silently")
	}
}

// TestCheckWorkflowMdShape_NoConfigDir — no config dir → OK skip.
func TestCheckWorkflowMdShape_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	check := h.checkWorkflowMdShape()
	if check.Status != "OK" {
		t.Errorf("status = %q, want OK skip", check.Status)
	}
}

// TestCheckWorkflowMdShape_FailureCollectsAllOffenders — multiple
// workflows missing description all surface in Items so the
// operator can fix them in one pass.
func TestCheckWorkflowMdShape_FailureCollectsAllOffenders(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Two without description, one with.
	mustWrite(t, filepath.Join(wfDir, "alpha.md"), `---
workflowId: alpha
entrypoint: go
steps:
  go:
    type: agent
    role: lead
    prompt: do it
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	mustWrite(t, filepath.Join(wfDir, "beta.md"), `---
workflowId: beta
entrypoint: go
steps:
  go:
    type: agent
    role: lead
    prompt: do it
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	mustWrite(t, filepath.Join(wfDir, "gamma.md"), `---
workflowId: gamma
description: "Healthy workflow with a description."
entrypoint: go
steps:
  go:
    type: agent
    role: lead
    prompt: do it
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	h := &DoctorHandlers{configDir: root}
	check := h.checkWorkflowMdShape()
	if check.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR", check.Status)
	}
	if len(check.Items) != 2 {
		t.Errorf("items = %d, want 2 offenders (alpha + beta)", len(check.Items))
	}
	joined := strings.Join(check.Items, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Errorf("items %v missing one of alpha/beta", check.Items)
	}
	if strings.Contains(joined, "gamma:") {
		t.Errorf("items %v includes healthy gamma", check.Items)
	}
}

// TestCheckWorkflowMdShape_AllHealthy — when every workflow carries
// a description the check reports OK with a count.
func TestCheckWorkflowMdShape_AllHealthy(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(wfDir, "ok.md"), `---
workflowId: ok
description: "Fine and dandy."
entrypoint: go
steps:
  go:
    type: agent
    role: lead
    prompt: do it
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`)
	h := &DoctorHandlers{configDir: root}
	check := h.checkWorkflowMdShape()
	if check.Status != "OK" {
		t.Errorf("status = %q, want OK; msg=%s items=%v", check.Status, check.Message, check.Items)
	}
}

// TestCheckWorkflowMdShape_LoadFailure_Errors — a malformed
// WORKFLOW.md surfaces as ERROR rather than crashing the check.
func TestCheckWorkflowMdShape_LoadFailure_Errors(t *testing.T) {
	root := t.TempDir()
	wfDir := filepath.Join(root, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(wfDir, "broken.md"), `no frontmatter here`)
	h := &DoctorHandlers{configDir: root}
	check := h.checkWorkflowMdShape()
	if check.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR on load failure", check.Status)
	}
}

// TestCheckWorkflowMdShape_EmptyWorkflowsDir — no workflows
// configured → OK noop.
func TestCheckWorkflowMdShape_EmptyWorkflowsDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := &DoctorHandlers{configDir: root}
	check := h.checkWorkflowMdShape()
	if check.Status != "OK" {
		t.Errorf("status = %q, want OK on empty workflows dir", check.Status)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
