package api

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Measured 2026-09-19: the WHOLE fail-open distribution over 41 hours of
// production uptime was step_not_found, 328 occurrences — and the executions
// behind it were running a workflow that is not their project's DEFAULT
// (`ingest` on a project that defaults to `adaptive`, `companion-rag-ingest`,
// `research`).
//
// The gate resolved the workflow from the PROJECT (GetProjectWithWorkflow
// returns project.DefaultWorkflowID), so every step id of a non-default
// workflow is unknown to it, every resolution fails, and every MCP call the
// agent makes resolves with NO role allowlist. The project gate still holds, so
// this is a role-scoping gap rather than a boundary breach — but the role's
// allowlist was silently not applied for an entire class of execution.

// gateWorkflows is the narrow lookup workflowForGate needs — the same shape
// stepForGate uses, and extracted for the same reason: the decision is a pure
// function of (what the execution says it runs, what the registry holds), and a
// pure function can be tested without standing up a registry and a repository.
type gateWorkflows map[string]*registry.Workflow

func (g gateWorkflows) GetWorkflow(id string) *registry.Workflow { return g[id] }

func gateFixture(execWorkflow, stepID string) (gateWorkflows, *persistence.Execution) {
	wfs := gateWorkflows{
		"adaptive": {ID: "adaptive", Steps: map[string]registry.WorkflowStep{"route": {Role: "dispatcher"}}},
		"ingest":   {ID: "ingest", Steps: map[string]registry.WorkflowStep{"recover": {Role: "ingester"}}},
	}
	step := stepID
	return wfs, &persistence.Execution{
		TaskID: "t1", ProjectID: "p1", WorkflowID: execWorkflow, CurrentStepID: &step,
	}
}

// The regression: an execution running a NON-DEFAULT workflow must resolve its
// role from the workflow it is actually running.
func TestWorkflowForGate_UsesTheExecutionsWorkflow(t *testing.T) {
	wfs, exec := gateFixture("ingest", "recover")

	wf, reason := workflowForGate(wfs, exec, "adaptive")
	if reason != mcpGapNone {
		t.Fatalf("a non-default workflow failed the gate open: reason=%v", reason)
	}
	if wf == nil || wf.ID != "ingest" {
		t.Fatalf("the gate resolved the wrong workflow: %+v", wf)
	}
	step, reason := stepForGate(wf, *exec.CurrentStepID)
	if reason != mcpGapNone || step.Role != "ingester" {
		t.Fatalf("the step did not resolve in the execution's own workflow: %v %+v", reason, step)
	}
}

// The default-workflow case keeps working, resolved the same way.
func TestWorkflowForGate_DefaultWorkflowStillResolves(t *testing.T) {
	wfs, exec := gateFixture("adaptive", "route")

	wf, reason := workflowForGate(wfs, exec, "adaptive")
	if reason != mcpGapNone || wf == nil || wf.ID != "adaptive" {
		t.Fatalf("the default workflow stopped resolving: %v %+v", reason, wf)
	}
}

// A step id that is genuinely unknown to the execution's OWN workflow is still
// step_not_found — the reason keeps meaning what it says, so the census can
// still answer a question rather than count a mood.
func TestWorkflowForGate_UnknownStepInOwnWorkflowStillReports(t *testing.T) {
	wfs, exec := gateFixture("ingest", "no-such-step")

	wf, reason := workflowForGate(wfs, exec, "adaptive")
	if reason != mcpGapNone {
		t.Fatalf("resolving the workflow failed: %v", reason)
	}
	if _, reason := stepForGate(wf, *exec.CurrentStepID); reason != mcpGapStepNotFound {
		t.Fatalf("want step_not_found, got %v", reason)
	}
}

// An execution whose workflow is not in the registry at all — deleted, or
// renamed under it — must report that it could not find the workflow rather
// than silently falling back to the project's default, which would resolve a
// role from a workflow this execution is not running.
func TestWorkflowForGate_MissingWorkflowDoesNotFallBackToDefault(t *testing.T) {
	wfs, exec := gateFixture("deleted-workflow", "recover")

	if _, reason := workflowForGate(wfs, exec, "adaptive"); reason != mcpGapNoWorkflow {
		t.Fatalf("want no_workflow, got %v — falling back to the default would apply another workflow's role grants", reason)
	}
}

// An execution that records NO workflow id predates the field or was written by
// a path that does not set it. Falling back to the project default is correct
// there and is the ONLY case where it is: there is nothing else to go on.
func TestWorkflowForGate_EmptyWorkflowIDFallsBackToTheDefault(t *testing.T) {
	wfs, exec := gateFixture("", "route")

	wf, reason := workflowForGate(wfs, exec, "adaptive")
	if reason != mcpGapNone || wf == nil || wf.ID != "adaptive" {
		t.Fatalf("an execution with no workflow id did not fall back: %v %+v", reason, wf)
	}
}
