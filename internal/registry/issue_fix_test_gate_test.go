package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIssueFixTestGateBeforePublish is the regression guard for the
// 2026-07-09 "red PR" incident: a feature whose unit tests fail (28
// ImportError failures, headmatch issue #36 → PR #37) reached a draft PR
// because the issue-fix path never actually RAN the test suite before
// publishing. The internal `review` step reviewed the diff only and
// rationalised the hard failures as intentional TDD "xfail" markers, then
// approved and published.
//
// The fix mirrors dev-pipeline: a `test` (tester) step sits between
// `decompose` (which delegates the subtask chain) and `review`, gating on
// testing.passed. No change can reach `review`/`publish` without the suite
// being run and green; a red suite routes to `remediate` (the bounded fix
// loop). remediate loops back through `test`, not straight to review, so a
// remediation can never skip the gate either.
//
// See https://docs.vornik.io and the
// RAG incident note companion_20260709204644_9bbc511d157b91be.
func TestIssueFixTestGateBeforePublish(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	path := filepath.Join(root, "configs", "workflows", "issue-fix.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	wf, err := ParseWorkflowMarkdown(content, path)
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown(issue-fix): %v", err)
	}

	// decompose must route into the test gate, NOT straight to review.
	decompose, ok := wf.Steps["decompose"]
	if !ok {
		t.Fatalf("issue-fix missing decompose step")
	}
	if decompose.OnSuccess != "test" {
		t.Errorf("decompose on_success = %q; want %q (must run tests before review/publish)", decompose.OnSuccess, "test")
	}

	// A tester step that actually runs the suite and gates on the result.
	test, ok := wf.Steps["test"]
	if !ok {
		t.Fatalf("issue-fix missing test step (the gate that runs the suite before publish)")
	}
	if test.Type != "agent" {
		t.Errorf("test type = %q; want %q", test.Type, "agent")
	}
	if test.Role != "tester" {
		t.Errorf("test role = %q; want %q", test.Role, "tester")
	}
	if test.OnFail != "failed" {
		t.Errorf("test on_fail = %q; want %q (a hard tester error opens no PR)", test.OnFail, "failed")
	}
	// An agent step's inline gates are evaluated ONLY when on_success is empty
	// (workflow.go short-circuits the gate block on OnSuccess). Setting it would
	// make the gate dead code and defeat the whole fix.
	if test.OnSuccess != "" {
		t.Errorf("test on_success = %q; want empty (a set on_success shadows the gates and makes them dead code)", test.OnSuccess)
	}
	assertGate(t, "test", test, "testing.passed == true", "review")
	assertGate(t, "test", test, "testing.passed == false", "remediate")

	// A remediation must be re-tested — it loops back through the gate, not
	// straight to review, so a fix can never bypass the suite.
	remediate, ok := wf.Steps["remediate"]
	if !ok {
		t.Fatalf("issue-fix missing remediate step")
	}
	if remediate.OnSuccess != "test" {
		t.Errorf("remediate on_success = %q; want %q (a remediation must be re-tested, not skip the gate)", remediate.OnSuccess, "test")
	}
}

func assertGate(t *testing.T, stepID string, step WorkflowStep, condition, target string) {
	t.Helper()
	for _, g := range step.Gates {
		if g.Condition == condition {
			if g.Target != target {
				t.Errorf("step %q gate %q target = %q; want %q", stepID, condition, g.Target, target)
			}
			return
		}
	}
	t.Errorf("step %q missing gate %q -> %q", stepID, condition, target)
}
