package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestRecoverExecution_AlreadyActiveIsNoop — if the task is already
// in activeExecutions (e.g. a duplicate recovery sweep), the helper
// returns nil without touching the repos.
func TestRecoverExecution_AlreadyActiveIsNoop(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1"})
	e.mu.Lock()
	e.activeExecutions["t1"] = &executionHandle{taskID: "t1"}
	e.mu.Unlock()
	exec := &persistence.Execution{ID: "e1", TaskID: "t1"}
	err := e.recoverExecution(context.Background(), exec)
	assert.NoError(t, err)
}

// TestRecoverExecution_TaskNotFoundErrors — the repo errors → the
// caller sees a wrapped error mentioning the task ID.
func TestRecoverExecution_TaskNotFoundErrors(t *testing.T) {
	e, _, _, _, tr := setup()
	tr.err = errors.New("db down")
	exec := &persistence.Execution{ID: "e1", TaskID: "missing"}
	err := e.recoverExecution(context.Background(), exec)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
}

// TestRecoverExecution_TaskAlreadyTerminalIsRecordedAsOrphaned — the
// task is already in a terminal status (COMPLETED/FAILED/CANCELLED),
// so we mark the orphaned execution row as failed and return nil
// without firing a goroutine.
func TestRecoverExecution_TaskAlreadyTerminalIsRecordedAsOrphaned(t *testing.T) {
	terminalStatuses := []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	}
	for _, status := range terminalStatuses {
		t.Run(string(status), func(t *testing.T) {
			e, _, er, _, tr := setup()
			task := &persistence.Task{ID: "t1", ProjectID: "p1", Status: status}
			tr.AddTask(task)
			exec := &persistence.Execution{ID: "e1", TaskID: "t1", ProjectID: "p1"}
			require.NoError(t, er.Create(context.Background(), exec))

			err := e.recoverExecution(context.Background(), exec)
			assert.NoError(t, err)
			// Task wasn't added to active executions (no recovery goroutine fired).
			assert.False(t, e.IsExecuting("t1"))
			// Execution record was marked failed with ORPHANED class.
			rec, _ := er.Get(context.Background(), "e1")
			require.NotNil(t, rec)
			assert.Equal(t, persistence.ExecutionStatusFailed, rec.Status)
		})
	}
}
