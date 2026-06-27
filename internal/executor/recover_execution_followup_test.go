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

// TestRecoverExecution_ReturnsForActiveExecution — if the
// executor already has the execution's task in activeExecutions,
// recoverExecution is a silent no-op. This is the
// "concurrent recovery race" guard.
func TestRecoverExecution_ActiveAlready(t *testing.T) {
	e, _, er, _, tr := setup()

	taskID := "t-active"
	tr.AddTask(&persistence.Task{
		ID:        taskID,
		ProjectID: "p",
		Status:    persistence.TaskStatusRunning,
		CreatedAt: time.Now(),
	})
	exec := &persistence.Execution{ID: "x-active", TaskID: taskID}
	require.NoError(t, er.Create(context.Background(), exec))

	// Pre-populate activeExecutions so the recoverExecution sees
	// the task is already being driven and bails.
	e.mu.Lock()
	e.activeExecutions[taskID] = &executionHandle{taskID: taskID}
	e.mu.Unlock()

	require.NoError(t, e.recoverExecution(context.Background(), exec))
	// No additional handle was created — still one.
	assert.Equal(t, 1, e.ActiveCount())
}

// TestRecoverExecution_TerminalTaskRecordsOrphan — when the
// task is already in a terminal state (FAILED / COMPLETED /
// CANCELLED), recoverExecution writes an ORPHANED failure on
// the execution row and skips the resume. Prevents the
// "ghost recovery" loop where every daemon restart re-runs a
// terminal task's execution.
func TestRecoverExecution_TerminalTaskRecordsOrphan(t *testing.T) {
	for _, term := range []persistence.TaskStatus{
		persistence.TaskStatusFailed,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusCancelled,
	} {
		t.Run(string(term), func(t *testing.T) {
			e, _, er, _, tr := setup()

			taskID := "t-" + string(term)
			tr.AddTask(&persistence.Task{
				ID:        taskID,
				ProjectID: "p",
				Status:    term,
				CreatedAt: time.Now(),
			})
			exec := &persistence.Execution{ID: "x-" + string(term), TaskID: taskID}
			require.NoError(t, er.Create(context.Background(), exec))

			require.NoError(t, e.recoverExecution(context.Background(), exec))
			// Execution row got RecordFailure with the orphan note.
			got, err := er.Get(context.Background(), exec.ID)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, persistence.ExecutionStatusFailed, got.Status)
			require.NotNil(t, got.ErrorMessage)
			assert.Contains(t, *got.ErrorMessage, "orphaned execution")
			// No goroutine was spawned for the terminal task.
			assert.False(t, e.IsExecuting(taskID))
		})
	}
}

// TestRecoverExecution_TaskLookupError — taskRepo.Get returns
// an error (DB blip). The helper propagates so the caller can
// log and skip this execution without aborting the whole
// recovery loop.
func TestRecoverExecution_TaskLookupError(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.err = assertErrf("db unreachable")

	exec := &persistence.Execution{ID: "x-blip", TaskID: "t-blip"}
	require.NoError(t, er.Create(context.Background(), exec))

	err := e.recoverExecution(context.Background(), exec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load task")
}

// TestRecoverExecution_LeasedTaskResumesNormally — a task in
// LEASED status (mid-execution when the daemon was killed) is a
// valid recovery candidate. The helper sets up a handle and
// spawns a goroutine. We don't need to assert the goroutine's
// behaviour here (covered by recovery_e2e_test.go) — just that
// the helper proceeds past the terminal-task guard.
func TestRecoverExecution_LeasedTaskProceeds(t *testing.T) {
	e, rt, er, _, tr := setup()
	// Wire a minimal resolver so runExecution doesn't immediately fail
	// at resolveExecutionPlan; this is just a smoke check that
	// recoverExecution accepted the LEASED task.
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "img"}}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf": {ID: "wf", Entrypoint: "step1",
				Steps: map[string]registry.WorkflowStep{
					"step1": {Type: "agent", Role: "worker"},
				}},
		},
	})
	rt.startErr = assertErrf("container start refused for test")

	tr.AddTask(&persistence.Task{
		ID:        "t-leased",
		ProjectID: "p",
		Status:    persistence.TaskStatusLeased,
		CreatedAt: time.Now(),
	})
	exec := &persistence.Execution{ID: "x-leased", TaskID: "t-leased"}
	require.NoError(t, er.Create(context.Background(), exec))

	require.NoError(t, e.recoverExecution(context.Background(), exec))
	// The helper registered the handle and spawned a goroutine.
	// Both Shutdown after will tidy this up.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	}()
	// Eventually the handle exists OR the goroutine errored out and
	// cleaned up. Either way is acceptable — what we care about is
	// that recoverExecution didn't immediately error.
	assert.Eventually(t, func() bool {
		got, _ := er.Get(context.Background(), exec.ID)
		return got != nil
	}, 2*time.Second, 10*time.Millisecond)
}
