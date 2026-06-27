//go:build integration

package postgres

import (
	"context"
	"errors"
	"hash/fnv"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Integration coverage for the telegram_task_threads table
// (migration v28) and TelegramThreadRepository. Run with:
//   go test -tags=integration ./internal/persistence/postgres/...
//
// Verifies:
//   - Insert + GetByTask round-trip.
//   - GetByThread looks up by (chat_id, thread_id).
//   - The (chat_id, thread_id) UNIQUE constraint surfaces as
//     ErrDuplicateKey so the bot's race-fallback path can re-resolve.
//   - MarkClosed stamps closed_at and is idempotent.
//   - ON DELETE CASCADE: deleting the parent task removes the row.

// uniqueThreadIDs derives a per-test (chat_id, thread_id) pair from
// the task ID so reruns don't collide on the UNIQUE constraint. The
// cleanup path is best-effort (the lifecycle tests have a known
// task_messages FK issue that blocks task DELETE), so collision
// resistance has to come from the inputs themselves.
func uniqueThreadIDs(taskID string) (int64, int64) {
	h := fnv.New64a()
	h.Write([]byte(taskID))
	v := int64(h.Sum64() & 0x7FFFFFFFFFFF) // 47-bit positive
	return -1000000000000 - v, int64(v & 0xFFFF)
}

func seedTaskForTelegramThread(t *testing.T, db *DB, taskID string) {
	t.Helper()
	ctx := context.Background()
	repo := NewTaskRepository(db)
	task := &persistence.Task{
		ID:        taskID,
		ProjectID: "test-project",
		Status:    persistence.TaskStatusPending,
		Priority:  5,
		Payload:   []byte(`{}`),
	}
	if err := repo.Create(ctx, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Delete the thread row before the task to avoid any ordering
	// surprises, and to guarantee the next rerun doesn't collide
	// on UNIQUE(chat_id, thread_id).
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM telegram_task_threads WHERE task_id = $1`, taskID)
		_, _ = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	})
}

func TestIntegration_TelegramThread_InsertAndGetByTask(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	taskID := "task_test_tgth_get_" + time.Now().Format("150405.000000")
	seedTaskForTelegramThread(t, db, taskID)
	repo := NewTelegramThreadRepository(db)

	chatID, threadID := uniqueThreadIDs(taskID)
	row := &persistence.TelegramTaskThread{
		TaskID:    taskID,
		ChatID:    chatID,
		ThreadID:  threadID,
		TopicName: taskID[len(taskID)-8:] + " — seed test topic",
	}
	if err := repo.Insert(ctx, row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row.ID == "" {
		t.Fatal("Insert must populate ID when caller leaves it empty")
	}

	got, err := repo.GetByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.ID != row.ID || got.TaskID != taskID || got.ChatID != row.ChatID ||
		got.ThreadID != row.ThreadID || got.TopicName != row.TopicName {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, row)
	}
	if got.ClosedAt != nil {
		t.Errorf("closed_at should be nil on fresh insert, got %v", got.ClosedAt)
	}
}

func TestIntegration_TelegramThread_GetByThread(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	taskID := "task_test_tgth_byth_" + time.Now().Format("150405.000000")
	seedTaskForTelegramThread(t, db, taskID)
	repo := NewTelegramThreadRepository(db)

	chatID, threadID := uniqueThreadIDs(taskID)
	row := &persistence.TelegramTaskThread{
		TaskID:    taskID,
		ChatID:    chatID,
		ThreadID:  threadID,
		TopicName: "another topic",
	}
	if err := repo.Insert(ctx, row); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.GetByThread(ctx, row.ChatID, row.ThreadID)
	if err != nil {
		t.Fatalf("GetByThread: %v", err)
	}
	if got.TaskID != taskID {
		t.Errorf("task_id: got %q want %q", got.TaskID, taskID)
	}

	// Unknown pair returns ErrNotFound — falls through to dispatcher.
	if _, err := repo.GetByThread(ctx, row.ChatID, row.ThreadID+9999); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown thread, got %v", err)
	}
}

func TestIntegration_TelegramThread_DuplicateConflict(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	taskA := "task_test_tgth_dupA_" + time.Now().Format("150405.000000")
	taskB := "task_test_tgth_dupB_" + time.Now().Format("150405.000000")
	seedTaskForTelegramThread(t, db, taskA)
	seedTaskForTelegramThread(t, db, taskB)
	repo := NewTelegramThreadRepository(db)

	chatID, threadID := uniqueThreadIDs(taskA)

	if err := repo.Insert(ctx, &persistence.TelegramTaskThread{
		TaskID: taskA, ChatID: chatID, ThreadID: threadID, TopicName: "first",
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Second insert with the same (chat_id, thread_id) must fail
	// with ErrDuplicateKey — that's the signal the bot's race
	// fallback uses to re-resolve via GetByTask.
	err := repo.Insert(ctx, &persistence.TelegramTaskThread{
		TaskID: taskB, ChatID: chatID, ThreadID: threadID, TopicName: "second",
	})
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey on (chat_id, thread_id) collision, got %v", err)
	}

	// The winner's row is unchanged.
	got, gerr := repo.GetByThread(ctx, chatID, threadID)
	if gerr != nil {
		t.Fatalf("GetByThread after conflict: %v", gerr)
	}
	if got.TaskID != taskA {
		t.Errorf("conflict winner should be taskA=%q, got %q", taskA, got.TaskID)
	}
}

func TestIntegration_TelegramThread_MarkClosed(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	taskID := "task_test_tgth_close_" + time.Now().Format("150405.000000")
	seedTaskForTelegramThread(t, db, taskID)
	repo := NewTelegramThreadRepository(db)

	chatID, threadID := uniqueThreadIDs(taskID)
	if err := repo.Insert(ctx, &persistence.TelegramTaskThread{
		TaskID: taskID, ChatID: chatID, ThreadID: threadID, TopicName: "x",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := repo.MarkClosed(ctx, taskID); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	got, err := repo.GetByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.ClosedAt == nil {
		t.Fatalf("closed_at must be set after MarkClosed")
	}
	firstClose := *got.ClosedAt

	// Idempotent: a second MarkClosed must not bump closed_at —
	// the timestamp records when the topic actually closed, not
	// the last terminal-event retry.
	time.Sleep(10 * time.Millisecond)
	if err := repo.MarkClosed(ctx, taskID); err != nil {
		t.Fatalf("MarkClosed (second): %v", err)
	}
	got2, err := repo.GetByTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetByTask after second close: %v", err)
	}
	if got2.ClosedAt == nil || !got2.ClosedAt.Equal(firstClose) {
		t.Errorf("MarkClosed must be idempotent; got %v want %v", got2.ClosedAt, firstClose)
	}
}

func TestIntegration_TelegramThread_CascadeOnTaskDelete(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	taskID := "task_test_tgth_casc_" + time.Now().Format("150405.000000")
	seedTaskForTelegramThread(t, db, taskID)
	repo := NewTelegramThreadRepository(db)

	chatID, threadID := uniqueThreadIDs(taskID)
	if err := repo.Insert(ctx, &persistence.TelegramTaskThread{
		TaskID: taskID, ChatID: chatID, ThreadID: threadID, TopicName: "y",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Drop the task. The FK is ON DELETE CASCADE so the thread row
	// must disappear too — otherwise we'd leak rows referencing
	// dead tasks.
	if _, err := db.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := repo.GetByTask(ctx, taskID); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("thread row should cascade-delete with task; got err=%v", err)
	}
}

func TestIntegration_TelegramThread_MigrationStructure(t *testing.T) {
	db := mustOpenForLifecycleTest(t)
	ctx := context.Background()

	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM information_schema.tables
		              WHERE table_name = 'telegram_task_threads')`,
	).Scan(&exists); err != nil || !exists {
		t.Fatalf("telegram_task_threads table missing (err=%v)", err)
	}

	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM pg_indexes
		              WHERE indexname = 'idx_telegram_task_threads_task')`,
	).Scan(&exists); err != nil || !exists {
		t.Errorf("idx_telegram_task_threads_task missing (err=%v)", err)
	}

	// UNIQUE(chat_id, thread_id) must be present — it's the race
	// guard for concurrent createForumTopic. Postgres exposes the
	// constraint as an auto-named unique index covering both
	// columns.
	var hasUnique bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM pg_constraint c
		  JOIN pg_class t ON c.conrelid = t.oid
		  WHERE t.relname = 'telegram_task_threads' AND c.contype = 'u'
		)`,
	).Scan(&hasUnique); err != nil || !hasUnique {
		t.Errorf("UNIQUE(chat_id, thread_id) constraint missing (err=%v)", err)
	}
}
