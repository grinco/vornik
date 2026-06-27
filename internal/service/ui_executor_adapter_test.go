package service

import (
	"testing"

	"vornik.io/vornik/internal/ui"
)

// TestUIExecutorAdapter_ImplementsTaskLogSource pins the
// regression discovered 2026-05-23: the adapter must satisfy
// ui.TaskLogSource so ui.WithExecutor wires s.taskLogSource. If
// this assertion ever returns false again, the live log panel
// on /ui/tasks/{id} silently returns "No logs available yet."
// for every task. The fix is a TaskLogs method that forwards
// to the underlying *executor.Executor.
func TestUIExecutorAdapter_ImplementsTaskLogSource(t *testing.T) {
	var a any = uiExecutorAdapter{}
	if _, ok := a.(ui.TaskLogSource); !ok {
		t.Fatalf("uiExecutorAdapter must implement ui.TaskLogSource — otherwise /ui/tasks/{id}/logs/stream returns the empty-fallback for every task")
	}
}
