package registry

// B-7 Phase 1 — RED tests for the `system` step type. Pin the
// YAML-parse + validate contract before any executor change. Also
// closes the drift bug where `a2a_call` (shipped 2026-05-25 in the
// A2A Phase B arc) wasn't in the validator allowlist.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeWorkflowYAML(t *testing.T, yaml string) string {
	t.Helper()
	tmp := t.TempDir()
	workflowsDir := filepath.Join(tmp, "workflows")
	if err := os.Mkdir(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "test.md"),
		[]byte("---\n"+yaml+"---\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return tmp
}

// TestWorkflowValidate_SystemStepAccepted — the new type loads
// when paired with a handler field. RED: validator rejects
// "system" today.
func TestWorkflowValidate_SystemStepAccepted(t *testing.T) {
	yaml := `workflowId: "doc-ingest-test"
entrypoint: "extract"
steps:
  extract:
    type: "system"
    handler: "rag.extract"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	tmp := writeWorkflowYAML(t, yaml)
	workflows, err := LoadWorkflows(tmp)
	if err != nil {
		t.Fatalf("expected system step to validate, got: %v", err)
	}
	wf := workflows["doc-ingest-test"]
	if wf == nil {
		t.Fatal("workflow not loaded")
	}
	step := wf.Steps["extract"]
	if step.Type != "system" {
		t.Errorf("step.Type = %q, want system", step.Type)
	}
	if step.Handler != "rag.extract" {
		t.Errorf("step.Handler = %q, want rag.extract", step.Handler)
	}
}

// TestWorkflowValidate_SystemStepMissingHandler — a system step
// without a handler field is a workflow-author error and must be
// caught at load time, not at execution time.
func TestWorkflowValidate_SystemStepMissingHandler(t *testing.T) {
	yaml := `workflowId: "broken"
entrypoint: "extract"
steps:
  extract:
    type: "system"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	tmp := writeWorkflowYAML(t, yaml)
	_, err := LoadWorkflows(tmp)
	if err == nil {
		t.Fatal("expected validation error for missing handler, got nil")
	}
}

// TestWorkflowValidate_A2ACallAccepted — a2a_call shipped
// 2026-05-25 but the validator never grew to include it.
// Workflows using it would fail to load — a latent regression.
// Pin the fix while we're widening validTypes for `system`.
func TestWorkflowValidate_A2ACallAccepted(t *testing.T) {
	yaml := `workflowId: "a2a-test"
entrypoint: "remote"
steps:
  remote:
    type: "a2a_call"
    agent_url: "http://example.com/a2a/v1/agents/p/wf"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	tmp := writeWorkflowYAML(t, yaml)
	_, err := LoadWorkflows(tmp)
	if err != nil {
		t.Fatalf("a2a_call should validate, got: %v", err)
	}
}

// TestWorkflowValidate_InvalidTypeStillRejected — sanity: widening
// validTypes for system + a2a_call must not silently pass an
// arbitrary typo through.
func TestWorkflowValidate_InvalidTypeStillRejected(t *testing.T) {
	yaml := `workflowId: "broken"
entrypoint: "x"
steps:
  x:
    type: "nonsense_type"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
`
	tmp := writeWorkflowYAML(t, yaml)
	_, err := LoadWorkflows(tmp)
	if err == nil {
		t.Fatal("expected validation error for bogus step type, got nil")
	}
}

// TestWorkflowValidate_DocumentIngestWorkflowParses — pins the
// shipped document-ingest workflow file. Once configs/workflows/
// document-ingest.md lands, this test loads the real fixture
// from disk so a future edit that breaks the YAML surfaces
// immediately. Skips when the file isn't present yet so the
// test is harmless until Phase 2.
func TestWorkflowValidate_DocumentIngestWorkflowParses(t *testing.T) {
	// Resolve repo-root → configs/workflows/document-ingest.md.
	// Tests run from internal/registry/; up three levels = repo
	// root. Skip cleanly if anything goes wrong rather than
	// false-failing on a path quirk.
	path, err := filepath.Abs("../../configs/workflows/document-ingest.md")
	if err != nil {
		t.Skipf("resolve repo path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("document-ingest.md not present yet (Phase 2): %v", err)
	}
	// Copy into a temp dir matching LoadWorkflows' expected
	// layout: <root>/workflows/*.md
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	tmp := t.TempDir()
	workflowsDir := filepath.Join(tmp, "workflows")
	if err := os.Mkdir(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "document-ingest.md"),
		body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	workflows, err := LoadWorkflows(tmp)
	if err != nil {
		t.Fatalf("document-ingest.md failed to load: %v", err)
	}
	wf := workflows["document-ingest"]
	if wf == nil {
		t.Fatal("document-ingest workflow not found after load")
	}
	// At least one step must be system-typed with a handler.
	var sawSystem bool
	for id, step := range wf.Steps {
		if step.Type == "system" {
			sawSystem = true
			if step.Handler == "" {
				t.Errorf("step %q has type=system but empty handler", id)
			}
		}
	}
	if !sawSystem {
		t.Error("document-ingest has no system-typed step (defeats B-7's no-LLM-cost goal)")
	}
}
