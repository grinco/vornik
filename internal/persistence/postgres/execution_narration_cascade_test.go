//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegration_ExecutionNarration_CascadeOnExecutionDelete pins
// the load-bearing retention guarantee from narrated-execution-
// design.md §5.3: narration rows die with their execution (FK
// ON DELETE CASCADE), so they never outlive the row they narrate
// and never need a separate sweep.
//
// Run with:  go test -tags=integration ./internal/persistence/postgres/...
func TestIntegration_ExecutionNarration_CascadeOnExecutionDelete(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000")
	taskID := "task_test_nar_casc_" + suffix
	execID := "exec_test_nar_casc_" + suffix
	projectID := "proj_test_nar_casc"

	taskRepo := NewTaskRepository(db)
	if err := taskRepo.Create(ctx, &persistence.Task{
		ID:        taskID,
		ProjectID: projectID,
		Status:    persistence.TaskStatusPending,
		Priority:  5,
		Payload:   []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM execution_narration WHERE task_id = $1`, taskID)
		_, _ = db.ExecContext(ctx, `DELETE FROM executions WHERE id = $1`, execID)
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	})

	if _, err := db.ExecContext(ctx, `
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status)
		VALUES ($1, $2, $3, 'wf', 'rev1', 'RUNNING')`,
		execID, taskID, projectID); err != nil {
		t.Fatalf("seed execution: %v", err)
	}

	repo := NewExecutionNarrationRepository(db)
	if _, err := repo.Insert(ctx, &persistence.ExecutionNarration{
		ID:          persistence.GenerateID("nar"),
		ProjectID:   projectID,
		TaskID:      taskID,
		ExecutionID: execID,
		Kind:        persistence.ExecutionNarrationKindStep,
		Text:        "Now researching — step 1",
	}); err != nil {
		t.Fatalf("insert narration: %v", err)
	}

	rows, err := repo.ListByExecution(ctx, execID)
	if err != nil {
		t.Fatalf("ListByExecution before delete: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 narration row before delete, got %d", len(rows))
	}

	// Drop the execution. The FK is ON DELETE CASCADE so the
	// narration row must disappear too.
	if _, err := db.ExecContext(ctx, `DELETE FROM executions WHERE id = $1`, execID); err != nil {
		t.Fatalf("delete execution: %v", err)
	}

	rows, err = repo.ListByExecution(ctx, execID)
	if err != nil {
		t.Fatalf("ListByExecution after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("narration rows should cascade-delete with the execution; got %d rows", len(rows))
	}
}
