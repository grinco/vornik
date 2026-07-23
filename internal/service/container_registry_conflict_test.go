package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/registry"
)

func newConflictTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedRunningExecution inserts a RUNNING execution referencing workflowID,
// along with its backing task row (executions.task_id references tasks.id).
func seedRunningExecution(t *testing.T, execRepo persistence.ExecutionRepository, taskRepo persistence.TaskRepository, id, projectID, workflowID string) {
	t.Helper()
	ctx := context.Background()

	taskID := id + "-task"
	task := &persistence.Task{
		ID:         taskID,
		ProjectID:  projectID,
		WorkflowID: &workflowID,
		Status:     persistence.TaskStatusRunning,
	}
	if err := taskRepo.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec := &persistence.Execution{
		ID:         id,
		TaskID:     taskID,
		ProjectID:  projectID,
		WorkflowID: workflowID,
		Status:     persistence.ExecutionStatusRunning,
	}
	if err := execRepo.Create(ctx, exec); err != nil {
		t.Fatalf("create execution: %v", err)
	}
}

func TestCollectExecutionConflicts_ValueOnlyWorkflowChange(t *testing.T) {
	// janka incident cpp_20260723033332: a reclaim-timeout edit to `research`
	// while an execution references it must NOT conflict — value-only edits
	// apply live. Only StructurallyChangedWorkflows conflict.
	ctx := context.Background()
	db := newConflictTestDB(t)
	execRepo := sqlite.NewExecutionRepository(db.DB)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	seedRunningExecution(t, execRepo, taskRepo, "exec1", "janka", "research") // helper: insert a RUNNING exec referencing workflow "research"

	valueOnly := registry.ConfigDiff{ChangedWorkflows: []string{"research"}}
	conf, err := collectExecutionConflicts(ctx, execRepo, taskRepo, valueOnly)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(conf) != 0 {
		t.Fatalf("value-only workflow change must not conflict, got %v", conf)
	}

	structural := registry.ConfigDiff{
		ChangedWorkflows:             []string{"research"},
		StructurallyChangedWorkflows: []string{"research"},
	}
	conf, err = collectExecutionConflicts(ctx, execRepo, taskRepo, structural)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(conf) == 0 {
		t.Fatal("structural workflow change MUST conflict with the in-flight execution")
	}
}
