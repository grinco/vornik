//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Regression coverage for the conversational task lifecycle's
// persistence layer. Three bugs encountered live during the
// 2026-05-09 Phase 23-32 rollout drive these tests:
//
//   1. scanTask was reading 21 columns; Phase 23 added 7 more
//      (brief_amended_at, current_phase, expected_by, closed_at,
//      closed_by, message_count, open_checkpoint_id). Result:
//      task.OpenCheckpointID was always nil after Get(), so
//      AnswerCheckpoint returned 409 even when the pointer was
//      set in the DB. Fix: extended scanTask + every SELECT
//      that calls it.
//
//   4. TransitionConditional passed []string directly to
//      lib/pq → "unsupported type []string, a slice of string".
//      Fix: pq.Array(statusStrings) wrap.
//
//   5. Migration v24 (enum extension) + v25 (partial indexes
//      using new enum values) were originally one migration.
//      Postgres rejected: "unsafe use of new value of enum type
//      task_status". Fix: split into two so the values are
//      committed before the index predicate references them.
//
// Run with:  go test -tags=integration ./internal/persistence/postgres/...

func mustOpenForLifecycleTest(t *testing.T) *DB {
	t.Helper()
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Register close as the first cleanup so it runs last (t.Cleanup is LIFO).
	// Data-cleanup callbacks registered by callers will therefore run before
	// the connection is closed.
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --------------------------------------------------------------
// Bug #5 — migration v24 + v25 split is structurally correct.
// --------------------------------------------------------------

func TestIntegration_Migration_TaskLifecycleStructure(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	// 1. All 12 task_status enum values present (the four new ones
	// are the regression target).
	rows, err := db.QueryContext(ctx, `
		SELECT enumlabel FROM pg_type t
		JOIN pg_enum e ON t.oid = e.enumtypid
		WHERE typname = 'task_status'
		ORDER BY enumsortorder`)
	if err != nil {
		t.Fatalf("enum query: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatal(err)
		}
		got[label] = true
	}
	want := []string{
		"PENDING", "QUEUED", "LEASED", "RUNNING", "WAITING_FOR_CHILDREN",
		"COMPLETED", "FAILED", "CANCELLED",
		"AWAITING_INPUT", "AWAITING_EXTERNAL", "PAUSED", "CLOSED",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("task_status enum missing value %q", w)
		}
	}

	// 2. New tables exist.
	for _, table := range []string{"task_messages", "task_scratchpad"} {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM information_schema.tables
			              WHERE table_name = $1)`, table).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}

	// 3. New columns on tasks.
	for _, col := range []string{
		"brief_amended_at", "current_phase", "expected_by",
		"closed_at", "closed_by", "message_count", "open_checkpoint_id",
	} {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM information_schema.columns
			              WHERE table_name = 'tasks' AND column_name = $1)`,
			col).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("tasks.%s column missing (err=%v)", col, err)
		}
	}

	// 4. Inbox partial indexes exist (created in v25 specifically
	// because they reference enum values added in v24 and the
	// combined migration would have been rejected).
	for _, idx := range []string{
		"idx_tasks_awaiting_input", "idx_tasks_awaiting_external",
		"idx_task_messages_task", "idx_task_messages_open_checkpoints",
	} {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)`,
			idx).Scan(&exists)
		if err != nil || !exists {
			t.Errorf("index %s missing (err=%v)", idx, err)
		}
	}
}

// --------------------------------------------------------------
// Bug #1 — scanTask must round-trip every Phase 23 column.
// --------------------------------------------------------------

func TestIntegration_TaskRepo_Phase23ColumnsRoundTrip(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()
	repo := NewTaskRepository(db)

	taskID := "task_test_p23_" + time.Now().Format("150405.000000")
	task := &persistence.Task{
		ID:        taskID,
		ProjectID: "test-project",
		Status:    persistence.TaskStatusPending,
		Priority:  10,
		Payload:   []byte(`{}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	})

	// Stamp Phase 23 columns directly so we can verify scanTask
	// reads them back. Mirrors what TransitionConditional does on
	// a real CLOSED transition + companion-column writes.
	now := time.Now().UTC().Truncate(time.Microsecond)
	closedBy := "vadim"
	currentPhase := "vendor_selection"
	openCheckpointID := "tmsg_checkpoint_xyz"

	// task_messages FK requires the row exist; insert a placeholder
	// checkpoint message first.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO task_messages (id, task_id, author_kind, message_kind, content)
		VALUES ($1, $2, 'lead', 'checkpoint', 'placeholder')`,
		openCheckpointID, taskID); err != nil {
		t.Fatalf("seed checkpoint message: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		UPDATE tasks SET
			brief_amended_at = $1,
			current_phase = $2,
			expected_by = $3,
			closed_at = $4,
			closed_by = $5,
			message_count = $6,
			open_checkpoint_id = $7
		WHERE id = $8`,
		now, currentPhase, now.Add(24*time.Hour), now, closedBy, 7, openCheckpointID,
		taskID); err != nil {
		t.Fatalf("update p23 cols: %v", err)
	}

	// Read back via the same scanTask path the API handlers use.
	got, err := repo.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Each Phase 23 column must round-trip. Pre-fix, ALL of these
	// asserted nil/zero because scanTask wasn't selecting them.
	if got.BriefAmendedAt == nil || !got.BriefAmendedAt.Equal(now) {
		t.Errorf("brief_amended_at: got %v want %v", got.BriefAmendedAt, now)
	}
	if got.CurrentPhase == nil || *got.CurrentPhase != currentPhase {
		t.Errorf("current_phase: got %v want %q", got.CurrentPhase, currentPhase)
	}
	if got.ExpectedBy == nil {
		t.Errorf("expected_by must round-trip; got nil")
	}
	if got.ClosedAt == nil {
		t.Errorf("closed_at must round-trip; got nil")
	}
	if got.ClosedBy == nil || *got.ClosedBy != closedBy {
		t.Errorf("closed_by: got %v want %q", got.ClosedBy, closedBy)
	}
	if got.MessageCount != 7 {
		t.Errorf("message_count: got %d want 7", got.MessageCount)
	}
	if got.OpenCheckpointID == nil || *got.OpenCheckpointID != openCheckpointID {
		t.Errorf("open_checkpoint_id: got %v want %q", got.OpenCheckpointID, openCheckpointID)
	}
}

// --------------------------------------------------------------
// Bug #4 — TransitionConditional must wrap []string with
// pq.Array (lib/pq doesn't auto-marshal a string slice).
// --------------------------------------------------------------

func TestIntegration_TransitionConditional_PqArrayWorks(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()
	repo := NewTaskRepository(db)

	taskID := "task_test_tx_" + time.Now().Format("150405.000000")
	task := &persistence.Task{
		ID:        taskID,
		ProjectID: "test-project",
		Status:    persistence.TaskStatusCompleted,
		Priority:  5,
		Payload:   []byte(`{}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	})

	// Multi-status `from` slice triggers the pq.Array path.
	// Pre-fix this returned: sql: converting argument $3 type:
	// unsupported type []string, a slice of string
	closer := "operator-vadim"
	ok, err := repo.TransitionConditional(ctx, taskID,
		[]persistence.TaskStatus{
			persistence.TaskStatusCompleted,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
		},
		persistence.TaskStatusClosed,
		persistence.TransitionOpts{
			ClosedBy:       &closer,
			SetClosedAtNow: true,
			ClearLease:     true,
		},
	)
	if err != nil {
		t.Fatalf("transition err=%v (regression: pq.Array wrap missing?)", err)
	}
	if !ok {
		t.Fatalf("transition should have applied (task was COMPLETED)")
	}

	// Verify all opts landed on the row.
	var status, gotClosedBy sql.NullString
	var closedAt sql.NullTime
	var leaseID sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT status, closed_by, closed_at, lease_id
		FROM tasks WHERE id = $1`, taskID).Scan(&status, &gotClosedBy, &closedAt, &leaseID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !status.Valid || status.String != string(persistence.TaskStatusClosed) {
		t.Errorf("status: got %v want CLOSED", status)
	}
	if !gotClosedBy.Valid || gotClosedBy.String != closer {
		t.Errorf("closed_by: got %v want %q", gotClosedBy, closer)
	}
	if !closedAt.Valid {
		t.Errorf("SetClosedAtNow must stamp closed_at")
	}
	if leaseID.Valid {
		t.Errorf("ClearLease must NULL the lease_id, got %q", leaseID.String)
	}
}

// --------------------------------------------------------------
// Bug #4 (companion) — concurrent / idempotent semantics.
// First write wins; second sees status drift and reports
// (false, nil).
// --------------------------------------------------------------

func TestIntegration_TransitionConditional_StatusDriftReturnsFalse(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()
	repo := NewTaskRepository(db)

	taskID := "task_test_drift_" + time.Now().Format("150405.000000")
	task := &persistence.Task{
		ID:        taskID,
		ProjectID: "test-project",
		Status:    persistence.TaskStatusCompleted,
		Priority:  5,
		Payload:   []byte(`{}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	})

	// First transition wins.
	ok, err := repo.TransitionConditional(ctx, taskID,
		[]persistence.TaskStatus{persistence.TaskStatusCompleted},
		persistence.TaskStatusClosed,
		persistence.TransitionOpts{SetClosedAtNow: true},
	)
	if err != nil || !ok {
		t.Fatalf("first transition: ok=%v err=%v", ok, err)
	}

	// Second transition (same from-set) sees CLOSED ≠ COMPLETED →
	// (false, nil), no error. This is what powers the API handlers'
	// 409 "task drifted out of close-eligible state" message.
	ok2, err2 := repo.TransitionConditional(ctx, taskID,
		[]persistence.TaskStatus{persistence.TaskStatusCompleted},
		persistence.TaskStatusClosed,
		persistence.TransitionOpts{SetClosedAtNow: true},
	)
	if err2 != nil {
		t.Errorf("status-drift transition should not error, got %v", err2)
	}
	if ok2 {
		t.Errorf("status-drift transition should return false (no row matched)")
	}
}
