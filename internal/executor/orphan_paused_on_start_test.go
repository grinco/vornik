package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestExecuteWithContext_SupersedesPriorNonTerminalExecutions — regression for
// the 2026-07-14 "three PAUSED cards, one paused task" incident
// (task_20260712021844_d6e14940dba6130b, project headmatch): every lease of a
// task creates a NEW execution row, but the task's prior non-terminal rows were
// only swept when the TASK reached a terminal status
// (cascadeOrphanExecutions). A task that parks (AWAITING_INPUT /
// WAITING_FOR_CHILDREN) and is resumed therefore left one orphan PAUSED
// execution behind per resume — and the fleet "Now Running" view renders one
// card per non-terminal EXECUTION, so a single paused task showed three PAUSED
// badges.
//
// The invariant: a task has at most one live execution. Starting a run must
// finalize any execution the task left behind.
//
// Create is scripted to fail so ExecuteWithContext returns before spawning the
// run goroutine — the assertion then lands squarely on the synchronous
// start-of-run sweep, and cannot be satisfied by the terminal-status cascade
// firing later from the workflow goroutine.
func TestExecuteWithContext_SupersedesPriorNonTerminalExecutions(t *testing.T) {
	e, _, er, _, tr := setup()

	// A task that parked on an operator checkpoint and was just resumed.
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusQueued,
		CreatedAt: time.Now(),
	})

	// The execution left behind by the pre-resume run: PAUSED, never finalized.
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{
		ID:        "exec_old",
		TaskID:    "t1",
		ProjectID: "p1",
		Status:    persistence.ExecutionStatusPaused,
	}))

	er.err = errors.New("create blocked")
	require.Error(t, e.ExecuteWithContext(context.Background(), "t1"))

	assert.Equal(t, persistence.ExecutionStatusCancelled, er.snapshotStatus("exec_old"),
		"the task's prior PAUSED execution must be superseded when the task starts a new run — "+
			"otherwise it lingers as a phantom card on the Now Running fleet view")
}

// TestExecuteWithContext_NoPriorExecutions_IsANoOp — the sweep must not be a
// precondition for starting a fresh task (no prior rows to finalize).
func TestExecuteWithContext_NoPriorExecutions_IsANoOp(t *testing.T) {
	e, _, er, _, tr := setup()
	tr.AddTask(&persistence.Task{
		ID:        "t1",
		ProjectID: "p1",
		Status:    persistence.TaskStatusQueued,
		CreatedAt: time.Now(),
	})

	er.err = errors.New("create blocked")
	require.Error(t, e.ExecuteWithContext(context.Background(), "t1"))

	n, err := er.SupersedeNonTerminalForTask(context.Background(), "t1")
	require.NoError(t, err)
	assert.Zero(t, n, "no executions existed, so nothing should be left to sweep")
}
