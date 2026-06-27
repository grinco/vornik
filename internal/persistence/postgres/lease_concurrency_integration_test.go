//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegrationLeaseTask_WaitingForChildrenDoesNotConsumeSlot pins
// the regression for the strict-adaptive deadlock: a parent task in
// WAITING_FOR_CHILDREN is yielding its runtime slot to the child it
// just spawned, so it must NOT count toward the project's
// maxConcurrentTasks cap. Without the fix, projects configured with
// cap=1 (the common case in this codebase) deadlock — the parent's
// WAITING row holds the only slot and the child stays QUEUED forever.
//
// Setup: project capped at 1, parent in WAITING_FOR_CHILDREN, child
// QUEUED. Expected: LeaseTask returns the child.
func TestIntegrationLeaseTask_WaitingForChildrenDoesNotConsumeSlot(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewTaskRepository(db.DB)

	const projectID = "test-wfc-cap"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id = $1`, projectID)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	parent := &persistence.Task{
		ID:          "test-wfc-parent",
		ProjectID:   projectID,
		Status:      persistence.TaskStatusWaitingForChildren,
		Priority:    50,
		Payload:     []byte(`{"taskType":"adaptive-parent"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   now,
	}
	child := &persistence.Task{
		ID:           "test-wfc-child",
		ProjectID:    projectID,
		ParentTaskID: &parent.ID,
		Status:       persistence.TaskStatusQueued,
		Priority:     50,
		Payload:      []byte(`{"taskType":"adaptive-child"}`),
		Attempt:      1,
		MaxAttempts:  1,
		CreatedAt:    now.Add(-1 * time.Minute),
		UpdatedAt:    now,
	}
	for _, task := range []*persistence.Task{parent, child} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}
	// Create() can clamp Status to QUEUED on insert paths; force the
	// parent into WAITING_FOR_CHILDREN directly so the lease query
	// sees the exact state strict-adaptive leaves behind.
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE tasks SET status = 'WAITING_FOR_CHILDREN' WHERE id = $1`, parent.ID); err != nil {
		t.Fatalf("force parent WAITING_FOR_CHILDREN: %v", err)
	}

	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:                projectID,
		LeaseHolder:              "test",
		LeaseDurationSeconds:     60,
		ProjectConcurrencyLimits: map[string]int{projectID: 1},
	})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased == nil {
		t.Fatal("expected child to be leased; got nil (parent's WAITING_FOR_CHILDREN consumed the only slot — strict-adaptive deadlock regressed)")
	}
	if leased.ID != child.ID {
		t.Errorf("leased %s, want %s", leased.ID, child.ID)
	}
}

// TestIntegrationLeaseTask_RunningParentBlocksChild is the
// counterpart: a parent in RUNNING (not yet delegated, not yet
// yielded) DOES occupy the project's slot, so its child must remain
// QUEUED. Pins the asymmetry so a careless fix to the WAITING case
// doesn't also drop RUNNING from the cap.
func TestIntegrationLeaseTask_RunningParentBlocksChild(t *testing.T) {
	cfg := Config{
		Host:           getEnvOrDefault("POSTGRES_HOST", "localhost"),
		Port:           integrationPort(),
		Database:       getEnvOrDefault("POSTGRES_DB", integrationDBName),
		User:           getEnvOrDefault("POSTGRES_USER", "vornik"),
		Password:       getEnvOrDefault("POSTGRES_PASSWORD", "vornik"),
		SSLMode:        "disable",
		ConnectTimeout: 10 * time.Second,
	}
	ctx := context.Background()
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewTaskRepository(db.DB)

	const projectID = "test-running-cap"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id = $1`, projectID)
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	parent := &persistence.Task{
		ID:          "test-running-parent",
		ProjectID:   projectID,
		Status:      persistence.TaskStatusQueued,
		Priority:    50,
		Payload:     []byte(`{"taskType":"adaptive-parent"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   now,
	}
	child := &persistence.Task{
		ID:           "test-running-child",
		ProjectID:    projectID,
		ParentTaskID: &parent.ID,
		Status:       persistence.TaskStatusQueued,
		Priority:     50,
		Payload:      []byte(`{"taskType":"adaptive-child"}`),
		Attempt:      1,
		MaxAttempts:  1,
		CreatedAt:    now.Add(-1 * time.Minute),
		UpdatedAt:    now,
	}
	for _, task := range []*persistence.Task{parent, child} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE tasks SET status = 'RUNNING' WHERE id = $1`, parent.ID); err != nil {
		t.Fatalf("force parent RUNNING: %v", err)
	}

	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:                projectID,
		LeaseHolder:              "test",
		LeaseDurationSeconds:     60,
		ProjectConcurrencyLimits: map[string]int{projectID: 1},
	})
	if err != nil && !errors.Is(err, persistence.ErrNoTasksAvailable) {
		t.Fatalf("lease: %v", err)
	}
	if leased != nil {
		t.Errorf("expected no lease (RUNNING parent consumes the only slot); got %s", leased.ID)
	}
}
