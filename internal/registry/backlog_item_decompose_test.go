package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBacklogItemDecomposeShape locks in the backlog-item v2 shape (2026-07-11):
// rewritten from the v1 monolithic analyze→implement loop, which flaked on
// feature-sized items (headmatch task …e457: container killed mid-work, rework
// loop exhausted, no-commit review fabrication). v2 mirrors issue-fix — a
// decompose step delegates the subtask chain (each in its own fresh worktree),
// a tester ACTUALLY RUNS the suite before review, and rejection/red tests loop
// through a bounded remediate step that re-tests. This guards against a
// regression to the monolithic shape or a set-on_success that shadows the gates.
func TestBacklogItemDecomposeShape(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	path := filepath.Join(root, "configs", "workflows", "backlog-item.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	wf, err := ParseWorkflowMarkdown(content, path)
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown(backlog-item): %v", err)
	}

	if wf.Entrypoint != "decompose" {
		t.Errorf("entrypoint = %q; want decompose (monolithic analyze entry was the v1 defect)", wf.Entrypoint)
	}
	if !wf.ResumeAfterChildren {
		t.Errorf("resume_after_children = false; want true (else the workflow won't resume after delegated subtasks)")
	}

	// decompose (lead) delegates the subtask chain under issue-subtask and
	// routes into the test gate.
	decompose, ok := wf.Steps["decompose"]
	if !ok {
		t.Fatalf("missing decompose step")
	}
	if decompose.Role != "lead" {
		t.Errorf("decompose role = %q; want lead", decompose.Role)
	}
	if decompose.OnSuccess != "test" {
		t.Errorf("decompose on_success = %q; want test", decompose.OnSuccess)
	}
	if decompose.DelegatedWorkflow != "issue-subtask" {
		t.Errorf("decompose delegated_workflow = %q; want issue-subtask (else subtasks fall back to dev-pipeline)", decompose.DelegatedWorkflow)
	}

	// tester step actually runs the suite; gates decide routing (on_success
	// empty, else the gates are dead code).
	test, ok := wf.Steps["test"]
	if !ok {
		t.Fatalf("missing test step")
	}
	if test.Role != "tester" || test.OnFail != "failed" {
		t.Errorf("test role/on_fail = %q/%q; want tester/failed", test.Role, test.OnFail)
	}
	if test.OnSuccess != "" {
		t.Errorf("test on_success = %q; want empty (a set on_success shadows the gates)", test.OnSuccess)
	}
	assertGate(t, "test", test, "testing.passed == true", "review")
	assertGate(t, "test", test, "testing.passed == false", "remediate")

	// reviewer step gates approved→publish, rejected→remediate.
	review, ok := wf.Steps["review"]
	if !ok {
		t.Fatalf("missing review step")
	}
	if review.OnSuccess != "" {
		t.Errorf("review on_success = %q; want empty", review.OnSuccess)
	}
	assertGate(t, "review", review, "review.approved == true", "publish")
	assertGate(t, "review", review, "review.approved == false", "remediate")

	// remediate re-tests (loops back through the gate), never straight to review.
	remediate, ok := wf.Steps["remediate"]
	if !ok {
		t.Fatalf("missing remediate step")
	}
	if remediate.OnSuccess != "test" {
		t.Errorf("remediate on_success = %q; want test (a remediation must be re-tested)", remediate.OnSuccess)
	}
	if remediate.DelegatedWorkflow != "issue-subtask" {
		t.Errorf("remediate delegated_workflow = %q; want issue-subtask", remediate.DelegatedWorkflow)
	}

	// publish is the deterministic forge step.
	publish, ok := wf.Steps["publish"]
	if !ok {
		t.Fatalf("missing publish step")
	}
	if publish.Type != "system" || publish.Handler != "forge.open_change_request" {
		t.Errorf("publish type/handler = %q/%q; want system/forge.open_change_request", publish.Type, publish.Handler)
	}
}
