package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// TestRecovery_AdoptsInFlightContainer_NoRespawn is the full crash-simulation
// integration test for executor crash-mid-step idempotency. It reconstructs
// the state an UNCLEAN daemon crash leaves behind — execution RUNNING at the
// entrypoint step, an in-flight record for a container that is still alive,
// and that container's result.json already on disk — then drives
// recoverExecution and asserts the step is ADOPTED, never re-spawned. A
// re-spawn (StartContainer) here would duplicate the step's side effects,
// which is exactly the bug this feature fixes (765874a6).
func TestRecovery_AdoptsInFlightContainer_NoRespawn(t *testing.T) {
	e, rt, er, _, tr := setup()
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "img"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf": {
				ID:         "wf",
				Entrypoint: "step1",
				Steps:      map[string]registry.WorkflowStep{"step1": {Type: "agent", Role: "worker", OnSuccess: "done"}},
				Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			},
		},
	})

	// The original (pre-crash) container already wrote its COMPLETED result.
	tempRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tempRoot, "output"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tempRoot, "output", "result.json"),
		[]byte(`{"status":"COMPLETED","message":"done by the original pre-crash run"}`), 0o644))

	// The container survived the daemon crash: InspectContainer finds it,
	// WaitForExit returns 0. StartContainer is left armed to FAIL — so if
	// recovery wrongly re-spawns, the test surfaces it as both a non-zero
	// StartCalls AND a step failure.
	rt.inspectByID = map[string]*runtime.Container{"c1": {Status: runtime.StatusRunning}}
	rt.waitCode = 0
	rt.startErr = assertErrf("re-spawn attempted — recovery should have adopted the in-flight container")

	// Execution row exactly as an unclean crash mid-step leaves it.
	st := executionState{
		CurrentStepID:       "step1",
		InFlightStepID:      "step1",
		InFlightContainerID: "c1",
		InFlightTempRoot:    tempRoot,
	}
	snap, err := json.Marshal(st)
	require.NoError(t, err)
	cs := "step1"
	exec := &persistence.Execution{
		ID: "x1", TaskID: "t1", ProjectID: "p",
		Status: persistence.ExecutionStatusRunning, StateSnapshot: snap, CurrentStepID: &cs,
	}
	require.NoError(t, er.Create(context.Background(), exec))
	tr.AddTask(&persistence.Task{
		ID: "t1", ProjectID: "p", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 3, CreatedAt: time.Now(),
	})

	require.NoError(t, e.recoverExecution(context.Background(), exec))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	}()

	require.Eventually(t, func() bool { return !e.IsExecuting("t1") }, 3*time.Second, 10*time.Millisecond)

	// The core guarantee: the step was adopted, NOT re-spawned.
	assert.Equal(t, 0, rt.StartCalls(),
		"recovery must adopt the in-flight container, not re-spawn it (StartContainer) — re-spawn duplicates side effects")
	// And the adopted result drove the workflow to COMPLETED.
	assert.Equal(t, persistence.ExecutionStatusCompleted, er.snapshotStatus("x1"),
		"the adopted container's result.json should complete the execution")
}
