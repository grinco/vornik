//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegrationLeaseTask_ProjectPriorityWins verifies LLD §4.2's
// primary sort key against a live database: when project A
// (priority 10) and project B (priority 50) both have eligible
// queued tasks, the lease drains project A's queue before project
// B gets a turn — strict priority across projects.
//
// Run with: go test -tags=integration ./internal/persistence/postgres/...
func TestIntegrationLeaseTask_ProjectPriorityWins(t *testing.T) {
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

	const prefix = "test-prio-"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id LIKE $1`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	highPrioProject := prefix + "high"
	lowPrioProject := prefix + "low"

	// Two tasks, low-priority project's task is OLDER (would win
	// FIFO) but its project priority is worse. Strict priority
	// should pick the high-priority project's task despite its
	// later created_at.
	lowPrioTask := &persistence.Task{
		ID:          "test-prio-low-task",
		ProjectID:   lowPrioProject,
		Status:      persistence.TaskStatusQueued,
		Priority:    50,
		Payload:     []byte(`{"taskType":"low"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   now,
	}
	highPrioTask := &persistence.Task{
		ID:          "test-prio-high-task",
		ProjectID:   highPrioProject,
		Status:      persistence.TaskStatusQueued,
		Priority:    50, // identical task-level priority — only project priority differs
		Payload:     []byte(`{"taskType":"high"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-1 * time.Minute), // newer than lowPrioTask
		UpdatedAt:   now,
	}
	for _, task := range []*persistence.Task{lowPrioTask, highPrioTask} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}

	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
		ProjectPriorities: map[string]int{
			highPrioProject: 10, // urgent
			lowPrioProject:  50, // background
		},
		ProjectPriorityDefault: 50,
	})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased == nil {
		t.Fatal("expected high-priority task to be leased; got nil")
	}
	if leased.ID != highPrioTask.ID {
		t.Errorf("leased %s, want %s — project priority must dominate FIFO ordering",
			leased.ID, highPrioTask.ID)
	}
}

// TestIntegrationLeaseTask_PriorityFallbackForUnknownProject pins
// the LEFT JOIN + COALESCE behaviour: a project missing from
// ProjectPriorities falls back to ProjectPriorityDefault, NOT to
// 0. Without the COALESCE, an unconfigured project would be
// promoted to the highest priority by default — exactly the
// opposite of what the LLD intends.
func TestIntegrationLeaseTask_PriorityFallbackForUnknownProject(t *testing.T) {
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

	const prefix = "test-fallback-"
	cleanup := func() {
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM tasks WHERE project_id LIKE $1`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	configuredProject := prefix + "configured"
	unconfiguredProject := prefix + "unconfigured"

	configuredTask := &persistence.Task{
		ID:          "test-fallback-configured",
		ProjectID:   configuredProject,
		Status:      persistence.TaskStatusQueued,
		Priority:    50,
		Payload:     []byte(`{"taskType":"configured"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-1 * time.Minute),
		UpdatedAt:   now,
	}
	unconfiguredTask := &persistence.Task{
		ID:          "test-fallback-unconfigured",
		ProjectID:   unconfiguredProject,
		Status:      persistence.TaskStatusQueued,
		Priority:    50,
		Payload:     []byte(`{"taskType":"unconfigured"}`),
		Attempt:     1,
		MaxAttempts: 1,
		CreatedAt:   now.Add(-2 * time.Minute), // older — would win FIFO
		UpdatedAt:   now,
	}
	for _, task := range []*persistence.Task{configuredTask, unconfiguredTask} {
		if err := repo.Create(ctx, task); err != nil {
			t.Fatalf("create %s: %v", task.ID, err)
		}
	}

	// configuredProject has priority 30 explicitly; unconfigured
	// falls back to ProjectPriorityDefault=50. configured wins
	// even though unconfiguredTask is older — without the
	// COALESCE, the missing project would default to NULL ASC =
	// before any number, promoting it to highest priority and
	// breaking the contract.
	leased, err := repo.LeaseTask(ctx, persistence.LeaseOptions{
		LeaseHolder:          "test",
		LeaseDurationSeconds: 60,
		ProjectPriorities: map[string]int{
			configuredProject: 30,
		},
		ProjectPriorityDefault: 50,
	})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased == nil {
		t.Fatal("expected configured task; got nil")
	}
	if leased.ID != configuredTask.ID {
		t.Errorf("leased %s, want configured (priority 30 < unconfigured fallback 50)",
			leased.ID)
	}
}
