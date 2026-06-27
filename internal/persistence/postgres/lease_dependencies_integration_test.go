//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegrationLeaseTask_DependencyGating pins the LLD §4.1
// dependency-satisfaction predicate against a live database. A task
// with one COMPLETED dependency AND one PENDING/FAILED/CANCELLED
// dependency must NOT be leased; once every dependency is COMPLETED,
// the task becomes eligible.
//
// Run with: go test -tags=integration ./internal/persistence/postgres/...
//
// Why integration not unit: the gate lives in a SQL CTE
// (`unnest(dependencies) … LEFT JOIN tasks d`). The semantics are
// only meaningful against a real Postgres — sqlmock-style assertions
// would only verify the string we generate, not that Postgres
// actually filters as intended (e.g. NULL handling for missing
// dependency rows).
func TestIntegrationLeaseTask_DependencyGating(t *testing.T) {
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

	// Clean slate. The test owns project IDs we can't accidentally
	// collide with real data — namespaced under a fixed prefix so a
	// developer running this against a shared vornik_test never
	// disturbs unrelated rows.
	const projectPrefix = "test-deps-"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id LIKE $1`, projectPrefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	pid := projectPrefix + "p1"

	// Three tasks: dep1 + dep2 are dependencies; gated has both
	// in dependencies[]. Use distinct created_at so FIFO ordering is
	// deterministic — gated is the YOUNGEST (created last) so
	// ordering wouldn't naturally pick it; only dependency
	// satisfaction can promote it.
	dep1 := &persistence.Task{
		ID:          "test-dep-1",
		ProjectID:   pid,
		Status:      persistence.TaskStatusCompleted, // pre-completed
		Priority:    50,
		Payload:     []byte(`{"taskType":"dep1"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-3 * time.Minute),
		UpdatedAt:   now,
	}
	dep2 := &persistence.Task{
		ID:          "test-dep-2",
		ProjectID:   pid,
		Status:      persistence.TaskStatusQueued, // NOT completed yet
		Priority:    50,
		Payload:     []byte(`{"taskType":"dep2"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   now,
	}
	gated := &persistence.Task{
		ID:           "test-gated",
		ProjectID:    pid,
		Status:       persistence.TaskStatusQueued,
		Priority:     50,
		Payload:      []byte(`{"taskType":"gated"}`),
		Attempt:      1,
		MaxAttempts:  1,
		Dependencies: []string{dep1.ID, dep2.ID},
		CreatedAt:    now.Add(-1 * time.Minute),
		UpdatedAt:    now,
	}
	for _, task := range []*persistence.Task{dep1, dep2, gated} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}

	// Phase 1: dep2 isn't COMPLETED yet, so the lease query should
	// pick dep2 (the only non-blocked QUEUED task at that priority)
	// and skip `gated` despite it being eligible by every other
	// criterion. ProjectID scope keeps the lease isolated from
	// parallel-running integration tests sharing the dedicated
	// vornik_integration_test database.
	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:            pid,
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
	})
	if err != nil {
		t.Fatalf("phase 1 lease: %v", err)
	}
	if leased == nil {
		t.Fatal("phase 1: expected dep2 to be leased; got nil")
	}
	if leased.ID != dep2.ID {
		t.Errorf("phase 1: leased %s, want dep2 — gated should NOT be leasable while dep2 is QUEUED", leased.ID)
	}

	// Mark dep2 COMPLETED to satisfy the gate.
	dep2.Status = persistence.TaskStatusCompleted
	if err := repo.Update(ctx, dep2); err != nil {
		t.Fatalf("complete dep2: %v", err)
	}

	// Phase 2: now `gated` is the only QUEUED task with all deps
	// satisfied — should lease cleanly.
	leased2, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		ProjectID:            pid,
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
	})
	if err != nil {
		t.Fatalf("phase 2 lease: %v", err)
	}
	if leased2 == nil {
		t.Fatal("phase 2: expected gated to be leased after deps complete; got nil")
	}
	if leased2.ID != gated.ID {
		t.Errorf("phase 2: leased %s, want gated", leased2.ID)
	}
}

// TestIntegrationLeaseTask_DependencyMissingDepRow covers the
// LEFT JOIN edge case: a task references a dependency ID that
// doesn't exist in the tasks table at all (e.g. a typo in the API
// payload, or a dep row deleted out from under the dependent).
// The LEFT JOIN's `d.id IS NULL` clause is what catches this:
// without it, a missing dep would slip through (because there's no
// row to be non-COMPLETED), which would be wrong — a task that
// references a phantom dep should NOT lease.
func TestIntegrationLeaseTask_DependencyMissingDepRow(t *testing.T) {
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

	const projectPrefix = "test-phantom-"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id LIKE $1`, projectPrefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	pid := projectPrefix + "p1"

	gated := &persistence.Task{
		ID:           "test-phantom-gated",
		ProjectID:    pid,
		Status:       persistence.TaskStatusQueued,
		Priority:     50,
		Payload:      []byte(`{"taskType":"phantom-gated"}`),
		Attempt:      1,
		MaxAttempts:  1,
		Dependencies: []string{"task-that-does-not-exist"},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := repo.Create(ctx, gated); err != nil {
		t.Fatalf("create gated: %v", err)
	}

	// LeaseTask must NOT return this task — the phantom dep ID has
	// no matching row, which the LEFT JOIN's d.id IS NULL clause
	// flags as "dependency not satisfied."
	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
		ProjectID:            pid, // scope the query so only this project's tasks are eligible
	})
	if err != nil && err != persistence.ErrNoTasksAvailable {
		t.Fatalf("lease: %v", err)
	}
	if leased != nil {
		t.Errorf("phantom-dep task should not be leased; got %s", leased.ID)
	}
}
