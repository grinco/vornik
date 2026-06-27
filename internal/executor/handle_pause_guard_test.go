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

// TestHandleFailure_RespectsPausedStatus: T-…1c44 regression
// guard. If the task has been flipped to PAUSED between the
// goroutine starting and the failure finaliser running, the
// terminal status write must be skipped — the operator's pause
// intent takes precedence over the in-flight failure.
func TestHandleFailure_RespectsPausedStatus(t *testing.T) {
	e, _, er, _, tr := setup()

	task := &persistence.Task{
		ID:          "t-paused",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning, // goroutine's snapshot
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	// Simulate the operator's pause having landed AFTER the
	// goroutine snapshotted `task` but BEFORE handleFailure runs.
	pausedTask := *task
	pausedTask.Status = persistence.TaskStatusPaused
	tr.tasks[task.ID] = &pausedTask

	exec := &persistence.Execution{
		ID:        "e-paused-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	// Run the failure path. The guard inside handleFailure
	// re-fetches the task; it should see PAUSED and bail.
	e.handleFailure(context.Background(), task, exec, errors.New("merge failed"))

	// Task status MUST still be PAUSED — not overwritten to FAILED.
	got, err := tr.Get(context.Background(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, got.Status,
		"handleFailure must not overwrite PAUSED with FAILED")

	// Execution status MUST still be RUNNING (or PAUSED) — not
	// RecordFailure'd to FAILED. The guard bails before any DB
	// writes happen on the execution side.
	execRow, err := er.Get(context.Background(), exec.ID)
	require.NoError(t, err)
	assert.NotEqual(t, persistence.ExecutionStatusFailed, execRow.Status,
		"handleFailure must not RecordFailure on a paused execution")
}

// TestHandleSuccess_RespectsPausedStatus: same guard for the
// success path. A goroutine that pauses between agent-completion
// and the merge step (or anywhere downstream) must not stamp
// COMPLETED over the operator's PAUSED.
func TestHandleSuccess_RespectsPausedStatus(t *testing.T) {
	e, _, er, _, tr := setup()

	task := &persistence.Task{
		ID:          "t-paused-success",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	pausedTask := *task
	pausedTask.Status = persistence.TaskStatusPaused
	tr.tasks[task.ID] = &pausedTask

	exec := &persistence.Execution{
		ID:        "e-paused-success-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleSuccess(context.Background(), task, exec, "container-1", []byte("{}"))

	got, err := tr.Get(context.Background(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusPaused, got.Status,
		"handleSuccess must not overwrite PAUSED with COMPLETED")

	execRow, err := er.Get(context.Background(), exec.ID)
	require.NoError(t, err)
	assert.NotEqual(t, persistence.ExecutionStatusCompleted, execRow.Status,
		"handleSuccess must not RecordCompletion on a paused execution")
}

// TestHandleFailure_NormalPathStillWritesFailed: positive
// control — the guard should ONLY fire when status is PAUSED.
// A normal RUNNING task still gets the terminal-FAILED write
// when retry budget is exhausted.
func TestHandleFailure_NormalPathStillWritesFailed(t *testing.T) {
	e, _, er, _, tr := setup()

	task := &persistence.Task{
		ID:          "t-normal-fail",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusRunning,
		Attempt:     3,
		MaxAttempts: 3,
		CreatedAt:   time.Now(),
	}
	tr.AddTask(task)
	exec := &persistence.Execution{
		ID:        "e-normal-fail-1",
		TaskID:    task.ID,
		ProjectID: task.ProjectID,
		Status:    persistence.ExecutionStatusRunning,
	}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), task, exec, errors.New("real failure"))

	got, err := tr.Get(context.Background(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusFailed, got.Status,
		"normal exhausted-retry path must still flip task.Status to FAILED")
}
