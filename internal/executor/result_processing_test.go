package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestHandleSelectedWorkflowRoute_NotFired pins the fall-through guard
// extracted in Track-B Phase 4: a step that is not a strict route with a
// candidate list reports fired=false so the loop continues to the normal
// post-step flow, leaving completedSteps untouched.
func TestHandleSelectedWorkflowRoute_NotFired(t *testing.T) {
	e := &Executor{}
	task := &persistence.Task{ID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	result := &agentStepResult{SelectedWorkflow: "research"}
	completed := []string{"route"}
	state := &executionState{}

	cases := []struct {
		name                string
		plan                *executionPlan
		routeAlreadyHandled bool
	}{
		{
			name:                "resume already handled never re-routes",
			plan:                &executionPlan{workflow: &registry.Workflow{ID: "adaptive", Entrypoint: "route"}, project: &registry.Project{AdaptiveCandidateWorkflows: []string{"research"}}},
			routeAlreadyHandled: true,
		},
		{
			name: "non-strict workflow",
			plan: &executionPlan{workflow: &registry.Workflow{ID: "dev-pipeline"}, project: &registry.Project{AdaptiveCandidateWorkflows: []string{"research"}}},
		},
		{
			name: "nil project",
			plan: &executionPlan{workflow: &registry.Workflow{ID: "adaptive", Entrypoint: "route"}, project: nil},
		},
		{
			name: "empty candidate list disables strict mode",
			plan: &executionPlan{workflow: &registry.Workflow{ID: "adaptive", Entrypoint: "route"}, project: &registry.Project{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fired, cid, res, gotCompleted, err := e.handleSelectedWorkflowRoute(
				context.Background(), task, exec, tc.plan, "route", registry.WorkflowStep{},
				0, &agentInputOpts{}, nil, result, tc.routeAlreadyHandled, state, completed, "cid-x", []byte("res-x"),
			)
			if fired {
				t.Fatalf("expected fired=false, got fired=true")
			}
			if cid != "" || res != nil {
				t.Fatalf("not-fired path must return zero container/result, got cid=%q res=%q", cid, res)
			}
			if len(gotCompleted) != 1 || gotCompleted[0] != "route" {
				t.Fatalf("completedSteps must be untouched, got %v", gotCompleted)
			}
			if err != nil {
				t.Fatalf("not-fired path must not error, got %v", err)
			}
		})
	}
}

// TestHandleDelegatedTasks_Empty pins the no-delegation guard: an agent result
// with no delegatedTasks reports fired=false and creates nothing.
func TestHandleDelegatedTasks_Empty(t *testing.T) {
	e, _, _, _, _ := setup()
	fired, err := e.handleDelegatedTasks(context.Background(),
		&persistence.Task{ID: "p1"}, &persistence.Execution{ID: "e1"},
		"step1", registry.WorkflowStep{}, &agentStepResult{}, &executionState{}, []string{"step1"})
	if fired {
		t.Fatal("empty delegatedTasks must report fired=false")
	}
	if err != nil {
		t.Fatalf("empty delegatedTasks must not error, got %v", err)
	}
}

// TestHandleDelegatedTasks_CreatesAndPauses exercises the full delegation +
// pause path extracted in Track-B Phase 4: children are created (with the
// step's DelegatedWorkflow pinned onto specs that omit it), the parent flips to
// WAITING_FOR_CHILDREN, and the execution is checkpointed paused.
func TestHandleDelegatedTasks_CreatesAndPauses(t *testing.T) {
	e, _, er, _, tr := setup()
	parent := &persistence.Task{ID: "parent-1", ProjectID: "proj-1", Status: persistence.TaskStatusRunning}
	tr.AddTask(parent)
	exec := &persistence.Execution{ID: "exec-1", TaskID: "parent-1"}
	_ = er.Create(context.Background(), exec)

	result := &agentStepResult{
		DelegationMode: "PARALLEL",
		DelegatedTasks: []delegatedTaskSpec{
			{Prompt: "child A", Role: "writer"},                       // no workflow → pinned
			{Prompt: "child B", Role: "writer", Workflow: "explicit"}, // keeps explicit
		},
	}
	state := &executionState{}
	step := registry.WorkflowStep{DelegatedWorkflow: "issue-subtask"}

	fired, err := e.handleDelegatedTasks(context.Background(), parent, exec, "decompose", step, result, state, []string{"decompose"})
	if !fired {
		t.Fatal("non-empty delegatedTasks must report fired=true")
	}
	if err != nil {
		t.Fatalf("delegation path must not error, got %v", err)
	}

	// DelegatedWorkflow pinned only where the spec omitted it.
	if result.DelegatedTasks[0].Workflow != "issue-subtask" {
		t.Fatalf("missing workflow must be pinned from step, got %q", result.DelegatedTasks[0].Workflow)
	}
	if result.DelegatedTasks[1].Workflow != "explicit" {
		t.Fatalf("explicit workflow must be preserved, got %q", result.DelegatedTasks[1].Workflow)
	}

	// Pause tail: parent flipped to awaiting-children + state pause reason set.
	got, _ := tr.Get(context.Background(), "parent-1")
	if got == nil || got.Status != persistence.TaskStatusWaitingForChildren {
		t.Fatalf("parent must flip to WAITING_FOR_CHILDREN, got %v", got)
	}
	if state.PausedReason != PauseReasonAwaitingChildren {
		t.Fatalf("state must record awaiting-children pause, got %q", state.PausedReason)
	}

	// Two children created.
	children, _ := tr.GetChildren(context.Background(), "parent-1")
	if len(children) != 2 {
		t.Fatalf("expected 2 delegated children, got %d", len(children))
	}
}
