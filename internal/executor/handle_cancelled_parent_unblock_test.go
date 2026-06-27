package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// TestExecutor_HandleCancelled_UnblocksWaitingParent — regression
// test for the 2026-06-07 child-cancel incident: handleCancelled
// (unlike handleSuccess/handleFailure) never called
// checkParentUnblock, so a parent in WAITING_FOR_CHILDREN whose
// last child was cancelled in-flight waited for it forever (until
// the parent itself was manually cancelled).
func TestExecutor_HandleCancelled_UnblocksWaitingParent(t *testing.T) {
	e, _, er, _, tr := setup()
	parentID := "parent-1"
	tr.AddTask(&persistence.Task{ID: parentID, ProjectID: "p1", Status: persistence.TaskStatusWaitingForChildren, CreatedAt: time.Now()})
	tr.AddTask(&persistence.Task{ID: "child-1", ProjectID: "p1", ParentTaskID: &parentID, Status: persistence.TaskStatusRunning, CreatedAt: time.Now()})
	exec := &persistence.Execution{ID: "e1", TaskID: "child-1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}
	_ = er.Create(context.Background(), exec)

	e.handleCancelled(context.Background(), tr.tasks["child-1"], exec)

	tr.mu.Lock()
	got := tr.tasks[parentID].Status
	tr.mu.Unlock()
	assert.Equal(t, persistence.TaskStatusQueued, got,
		"cancelling the last child must re-queue the WAITING_FOR_CHILDREN parent")
}
