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

// TestExecuteWorkflowAttempt_TerminalCancelled — workflow's
// entrypoint IS a CANCELLED terminal. The helper returns the
// "cancelled" error so the caller bails without spawning any
// agent step.
func TestExecuteWorkflowAttempt_TerminalCancelled(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID:         "wf",
			Entrypoint: "cancelled_terminal",
			Terminals: map[string]registry.WorkflowTerminal{
				"cancelled_terminal": {Status: "CANCELLED"},
			},
			Steps: map[string]registry.WorkflowStep{},
		},
	}
	exec := &persistence.Execution{
		ID:     "x-can",
		TaskID: "t",
		Status: persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, completed, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflow cancelled")
	assert.Empty(t, completed, "no agent steps executed; completedSteps empty")
}

// TestExecuteWorkflowAttempt_UnsupportedTerminalStatus — when
// the terminal's Status field is something the executor doesn't
// recognise (typo: "DONE", "PAUSED" — neither is valid), the
// helper surfaces a specific error so the operator sees the
// typo and not a generic "unknown failure".
func TestExecuteWorkflowAttempt_UnsupportedTerminalStatus(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID:         "wf",
			Entrypoint: "weird_terminal",
			Terminals: map[string]registry.WorkflowTerminal{
				"weird_terminal": {Status: "DONE"}, // not a valid terminal
			},
			Steps: map[string]registry.WorkflowStep{},
		},
	}
	exec := &persistence.Execution{ID: "x-unsup", TaskID: "t"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported terminal status")
	assert.Contains(t, err.Error(), "DONE")
}

// TestExecuteWorkflowAttempt_StepNotFound — entrypoint points at
// a step that doesn't exist in the workflow's Steps map (config
// error). The helper surfaces a clear error.
func TestExecuteWorkflowAttempt_StepNotFound(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID:         "wf",
			Entrypoint: "missing_step",
			Terminals:  map[string]registry.WorkflowTerminal{},
			Steps:      map[string]registry.WorkflowStep{}, // entrypoint not present
		},
	}
	exec := &persistence.Execution{ID: "x-nf", TaskID: "t"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_step")
}

// TestExecuteWorkflowAttempt_MaxIterationsExceeded — a workflow
// with a loop that revisits steps must bail when total
// iterations exceeds MaxIterations. Drive via a tight self-loop
// step that always succeeds and routes back to itself.
func TestExecuteWorkflowAttempt_MaxIterationsExceeded(t *testing.T) {
	rt := NewMockRuntime()
	// Each container start succeeds and writes a clean result so the
	// loop keeps going.
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID:            "wf",
			Entrypoint:    "loopy",
			MaxIterations: 2,
			MaxStepVisits: 100, // high enough that iteration cap wins
			Steps: map[string]registry.WorkflowStep{
				"loopy": {
					Type:      "agent",
					Role:      "w",
					OnSuccess: "loopy", // self-loop
				},
			},
			Terminals: map[string]registry.WorkflowTerminal{},
		},
	}
	exec := &persistence.Execution{ID: "x-loop", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iterations",
		"either total-iterations or per-step-visit cap must trigger an error")
}

// TestExecuteWorkflowAttempt_MaxStepVisitsExceeded — the
// per-step visit cap (default 3) fires before MaxIterations
// when MaxIterations is large. Drives a self-loop with the
// per-step cap as the tight guard.
func TestExecuteWorkflowAttempt_MaxStepVisits(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID:            "wf",
			Entrypoint:    "loopy",
			MaxIterations: 1000,
			MaxStepVisits: 2, // tight per-step cap
			Steps: map[string]registry.WorkflowStep{
				"loopy": {Type: "agent", Role: "w", OnSuccess: "loopy"},
			},
		},
	}
	exec := &persistence.Execution{ID: "x-svisits", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "visited",
		"per-step visit cap must surface 'visited N times' diagnostic")
	assert.Contains(t, err.Error(), "loopy",
		"the offending step ID must appear in the error")
}
