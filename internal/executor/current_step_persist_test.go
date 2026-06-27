package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestRunExecution_PersistsCurrentStepDuringFirstStep is the regression
// for the 2026-06-11 report that /ui/executions/<id> showed an empty
// "Step Progress" panel while the FIRST step was running. The detail
// template only renders the live step block when current_step_id is set,
// but the executor previously persisted current_step_id only AFTER a step
// completed (as the NEXT step's id). The entrypoint step therefore had no
// persisted current_step_id for the entire duration of its run, so the
// panel was blank until step 1 finished. The executor must publish the
// step it is ABOUT to run before running it.
func TestRunExecution_PersistsCurrentStepDuringFirstStep(t *testing.T) {
	rt := NewMockRuntime()
	gate := make(chan struct{})
	entered := make(chan struct{})
	rt.waitGate = gate
	rt.waitEntered = entered
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "research",
				Steps:      map[string]registry.WorkflowStep{"research": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})
	tr.AddTask(&persistence.Task{
		ID:          "t1",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	})

	require.NoError(t, e.Execute("t1"))

	// Wait until the first step's container has started and the executor
	// is blocked in WaitForExit — i.e. the entrypoint step is actively
	// running. This is exactly the window the UI renders as "running".
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first step never reached WaitForExit")
	}

	exec, err := er.GetByTaskID(context.Background(), "t1")
	require.NoError(t, err)
	require.NotNil(t, exec)
	if assert.NotNil(t, exec.CurrentStepID, "current_step_id must be persisted while the first step runs — else the detail page Step Progress panel is blank") {
		assert.Equal(t, "research", *exec.CurrentStepID,
			"current_step_id must point at the entrypoint step that is running")
	}

	close(gate)
	require.Eventually(t, func() bool {
		return !e.IsExecuting("t1")
	}, 2*time.Second, 10*time.Millisecond)
}
