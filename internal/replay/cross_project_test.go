package replay

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// fakeCPCLister is the test stub for crossProjectCallLister.
// Captures the row keyed by both id and callee_task_id so the
// Builder's incoming + outgoing lookups both work.
type fakeCPCLister struct {
	byID           map[string]*persistence.CrossProjectCall
	byCalleeTaskID map[string]*persistence.CrossProjectCall
}

func (f *fakeCPCLister) Get(_ context.Context, id string) (*persistence.CrossProjectCall, error) {
	if r, ok := f.byID[id]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeCPCLister) GetByCalleeTaskID(_ context.Context, calleeTaskID string) (*persistence.CrossProjectCall, error) {
	if r, ok := f.byCalleeTaskID[calleeTaskID]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

type fakeSpawnLister struct {
	bySpawned map[string]*persistence.ProjectSpawn
}

func (f *fakeSpawnLister) GetBySpawnedProject(_ context.Context, slug string) (*persistence.ProjectSpawn, error) {
	if r, ok := f.bySpawned[slug]; ok {
		return r, nil
	}
	return nil, persistence.ErrNotFound
}

type fakeChildLister struct {
	byParent map[string][]*persistence.Task
}

func (f *fakeChildLister) GetChildren(_ context.Context, parentID string) ([]*persistence.Task, error) {
	return f.byParent[parentID], nil
}

type fakeExecByTaskGetter struct {
	byTask map[string]*persistence.Execution
}

func (f *fakeExecByTaskGetter) GetByTaskID(_ context.Context, taskID string) (*persistence.Execution, error) {
	if e, ok := f.byTask[taskID]; ok {
		return e, nil
	}
	return nil, persistence.ErrNotFound
}

// TestCollectIncomingCrossProjectCall_BuildsBreadcrumb covers
// the "task was called from another project" path. The
// breadcrumb should carry the caller-project name + a deep-
// link back to the caller's execution.
func TestCollectIncomingCrossProjectCall_BuildsBreadcrumb(t *testing.T) {
	cpcID := "ccp_1"
	cpc := &persistence.CrossProjectCall{
		ID:             cpcID,
		CallerTaskID:   "task-caller",
		CallerStepID:   "step-handoff",
		CallerProject:  "marketing",
		CalleeProject:  "architect",
		CalleeWorkflow: "produce-spec",
		Status:         persistence.CPCStatusRunning,
		CreatedAt:      time.Now(),
	}
	builder := &Builder{
		CrossProjectCalls: &fakeCPCLister{byID: map[string]*persistence.CrossProjectCall{cpcID: cpc}},
		ExecutionByTask: &fakeExecByTaskGetter{byTask: map[string]*persistence.Execution{
			"task-caller": {ID: "exec-caller", TaskID: "task-caller"},
		}},
	}
	task := &persistence.Task{ID: "task-callee", CrossProjectCallID: &cpcID}

	hop := builder.collectIncomingCrossProjectCall(context.Background(), task)
	if hop == nil {
		t.Fatal("expected non-nil breadcrumb")
	}
	if hop.Kind != "call" {
		t.Errorf("Kind = %q, want call", hop.Kind)
	}
	if hop.CalleeProject != "marketing" {
		t.Errorf("CalleeProject (= other-end label) = %q, want marketing", hop.CalleeProject)
	}
	if hop.CalleeURL != "/ui/executions/exec-caller/replay" {
		t.Errorf("CalleeURL = %q", hop.CalleeURL)
	}
	if hop.CallStatus != string(persistence.CPCStatusRunning) {
		t.Errorf("CallStatus = %q, want running", hop.CallStatus)
	}
}

// TestCollectIncomingCrossProjectCall_NilForNonCallee asserts
// the breadcrumb is nil for ordinary tasks (the common case —
// most tasks aren't CPC callees).
func TestCollectIncomingCrossProjectCall_NilForNonCallee(t *testing.T) {
	builder := &Builder{
		CrossProjectCalls: &fakeCPCLister{byID: map[string]*persistence.CrossProjectCall{}},
	}
	task := &persistence.Task{ID: "ordinary-task"}
	if hop := builder.collectIncomingCrossProjectCall(context.Background(), task); hop != nil {
		t.Errorf("expected nil breadcrumb for non-callee task, got %+v", hop)
	}
}

// TestCollectOutboundHops_CallAndSpawn covers both edge types
// on a task that issued one call_project + one spawn_project.
// Children of the parent task carry the linkage:
//   - CPC callee child has CrossProjectCallID set
//   - spawn initial_task child has ProjectID matching a
//     project_spawns row
func TestCollectOutboundHops_CallAndSpawn(t *testing.T) {
	cpcID := "ccp_1"
	cpc := &persistence.CrossProjectCall{
		ID: cpcID, CallerStepID: "handoff", CalleeProject: "architect",
		CalleeWorkflow: "produce-spec", ExpectedSchema: "spec_envelope.v1",
		Status: persistence.CPCStatusCompleted,
	}
	spawn := &persistence.ProjectSpawn{
		ID: "ps_1", ParentTaskID: "task-parent", ParentProject: "marketing",
		ParentStepID: "launch", SpawnedProject: "sales-q3", TemplateSlug: "sales-campaign",
	}

	callChild := &persistence.Task{
		ID: "task-architect-child", CrossProjectCallID: &cpcID,
		ProjectID: "architect",
	}
	spawnChild := &persistence.Task{
		ID:        "task-sales-initial",
		ProjectID: "sales-q3",
	}

	builder := &Builder{
		CrossProjectCalls: &fakeCPCLister{byID: map[string]*persistence.CrossProjectCall{cpcID: cpc}},
		ProjectSpawns:     &fakeSpawnLister{bySpawned: map[string]*persistence.ProjectSpawn{"sales-q3": spawn}},
		TaskChildren: &fakeChildLister{byParent: map[string][]*persistence.Task{
			"task-parent": {callChild, spawnChild},
		}},
		ExecutionByTask: &fakeExecByTaskGetter{byTask: map[string]*persistence.Execution{
			"task-architect-child": {ID: "exec-architect"},
			"task-sales-initial":   {ID: "exec-sales"},
		}},
	}

	hops := builder.collectOutboundCrossProjectHops(context.Background(), &persistence.Task{ID: "task-parent"})
	if len(hops) != 2 {
		t.Fatalf("expected 2 outbound hops, got %d", len(hops))
	}

	// Categorise by kind for stable assertions.
	var call, spawnH *CrossProjectHop
	for i := range hops {
		switch hops[i].Kind {
		case "call":
			call = &hops[i]
		case "spawn":
			spawnH = &hops[i]
		}
	}
	if call == nil || spawnH == nil {
		t.Fatalf("expected one call + one spawn hop, got %+v", hops)
	}

	if call.CalleeProject != "architect" || call.CalleeURL != "/ui/executions/exec-architect/replay" {
		t.Errorf("call hop wrong: %+v", call)
	}
	if call.ExpectedSchema != "spec_envelope.v1" || call.CallStatus != "completed" {
		t.Errorf("call hop missing schema/status: %+v", call)
	}

	if spawnH.SpawnedProject != "sales-q3" || spawnH.TemplateSlug != "sales-campaign" {
		t.Errorf("spawn hop routing wrong: %+v", spawnH)
	}
	if spawnH.CalleeURL != "/ui/executions/exec-sales/replay" {
		t.Errorf("spawn hop URL = %q", spawnH.CalleeURL)
	}
}

// TestCollectOutboundHops_NilDepsAreSafe covers the degraded
// configuration where the cross-project repos aren't wired
// (e.g. SQLite branch). The function must return an empty
// slice without panicking.
func TestCollectOutboundHops_NilDepsAreSafe(t *testing.T) {
	builder := &Builder{} // all fields nil
	hops := builder.collectOutboundCrossProjectHops(context.Background(), &persistence.Task{ID: "task-parent"})
	if hops != nil {
		t.Errorf("expected nil hops on unwired builder, got %+v", hops)
	}
}
