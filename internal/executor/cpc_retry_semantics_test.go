package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestCallProject_RetrySemantics_CreatesNewCPC asserts that
// re-executing the same call_project step (retry-from-step or
// scheduler recovery) creates a NEW CPC row rather than
// reusing the prior one. The LLD §8.4 contract:
//
//	"Retry-from-step on a `call_project` step: the existing
//	 retry pipeline supersedes prior step outcomes and re-runs.
//	 The new CPC creates a NEW row (NEW id, new callee task).
//	 The PRIOR callee task remains in its terminal state — it
//	 was real work, may have side effects, can't be untaken."
//
// This test pins the "new row per execution" behaviour. Two
// sequential calls with identical step + caller-task IDs must
// produce two distinct CPC rows; the executor's existing
// supersede-outcomes pass closes off the prior row's lineage
// while leaving the original CPC intact for audit.
func TestCallProject_RetrySemantics_CreatesNewCPC(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"architect": {ID: "architect", AcceptCallsFrom: []string{"marketing"}},
		},
	}
	e, _ := newCallProjectExecutor(resolver, cpc)

	task := &persistence.Task{ID: "task-caller", ProjectID: "marketing"}
	exec := &persistence.Execution{ID: "exec-caller"}

	first, err := e.handleCallProjectStep(context.Background(), task, exec, "step-handoff", makeCallStep(), nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := e.handleCallProjectStep(context.Background(), task, exec, "step-handoff", makeCallStep(), nil)
	if err != nil {
		t.Fatalf("retry call: %v", err)
	}

	if first.CPCId == second.CPCId {
		t.Errorf("retry should mint a new CPC, both = %q", first.CPCId)
	}
	if first.CalleeTaskID == second.CalleeTaskID {
		t.Errorf("retry should mint a new callee task, both = %q", first.CalleeTaskID)
	}
	if len(cpc.rows) != 2 {
		t.Errorf("expected 2 CPC rows after retry, got %d", len(cpc.rows))
	}
}

// spawn-side retry-from-step semantics are covered by
// TestSpawnProject_Idempotent in spawn_project_test.go — the
// existing test seeds an existing spawn and asserts the second
// handler call returns Skipped=true without re-materialising.
// LLD §6.2's idempotence contract is therefore already
// regression-pinned; no additional test needed here.
