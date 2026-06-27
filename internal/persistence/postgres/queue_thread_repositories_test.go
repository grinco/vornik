package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

func ingestQueueRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "project_id", "source_artifact_id", "producer_role",
		"ingest_execution_id", "priority", "proposed_class",
		"proposed_confidence", "state", "attempts", "enqueued_at",
		"started_at", "finished_at", "last_error", "repo_scope",
	})
}

func TestTaskWatcherRepositoryCRUD(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskWatcherRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_watchers")).
		WithArgs("task-1", int64(123), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Watch(context.Background(), "task-1", 123); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT chat_id FROM task_watchers WHERE task_id = $1")).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{"chat_id"}).AddRow(int64(123)).AddRow(int64(456)))
	got, err := repo.GetWatchers(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetWatchers() error = %v", err)
	}
	if len(got) != 2 || got[0] != 123 || got[1] != 456 {
		t.Fatalf("GetWatchers() = %#v", got)
	}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM task_watchers WHERE task_id = $1")).
		WithArgs("task-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := repo.RemoveWatchers(context.Background(), "task-1"); err != nil {
		t.Fatalf("RemoveWatchers() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestTelegramThreadRepositoryLifecycle(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTelegramThreadRepository(db)

	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("expected nil thread error")
	}
	if _, err := repo.GetByTask(context.Background(), ""); err == nil {
		t.Fatal("expected empty task id error")
	}
	if _, err := repo.GetByThread(context.Background(), 0, 42); err != persistence.ErrNotFound {
		t.Fatalf("GetByThread invalid = %v, want ErrNotFound", err)
	}
	if err := repo.MarkClosed(context.Background(), ""); err == nil {
		t.Fatal("expected empty task id error")
	}

	thread := &persistence.TelegramTaskThread{
		TaskID: "task-1", ChatID: -100, ThreadID: 77, TopicName: "Task 1",
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO telegram_task_threads")).
		WithArgs(sqlmock.AnyArg(), thread.TaskID, thread.ChatID, thread.ThreadID, thread.TopicName, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Insert(context.Background(), thread); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
	if thread.ID == "" || thread.CreatedAt.IsZero() {
		t.Fatalf("Insert() did not set defaults: %#v", thread)
	}

	created := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	closed := created.Add(time.Hour)
	rows := sqlmock.NewRows([]string{"id", "task_id", "chat_id", "thread_id", "topic_name", "created_at", "closed_at"}).
		AddRow("tgth-1", "task-1", int64(-100), int64(77), "Task 1", created, closed)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at")).
		WithArgs("task-1").
		WillReturnRows(rows)
	got, err := repo.GetByTask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetByTask() error = %v", err)
	}
	if got.ID != "tgth-1" || got.ClosedAt == nil || !got.ClosedAt.Equal(closed) {
		t.Fatalf("GetByTask() = %#v", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, chat_id, thread_id, topic_name, created_at, closed_at")).
		WithArgs(int64(-100), int64(77)).
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetByThread(context.Background(), -100, 77); err != persistence.ErrNotFound {
		t.Fatalf("GetByThread missing = %v, want ErrNotFound", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE telegram_task_threads SET closed_at = now()")).
		WithArgs("task-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkClosed(context.Background(), "task-1"); err != nil {
		t.Fatalf("MarkClosed() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestChunkGraphExtractionRepositoryStatsAndState(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewChunkGraphExtractionRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, content")).
		WithArgs(50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "content"}).
			AddRow("chunk-1", "proj-a", "content"))
	chunks, err := repo.FetchUnextracted(context.Background(), 0)
	if err != nil {
		t.Fatalf("FetchUnextracted() error = %v", err)
	}
	if len(chunks) != 1 || chunks[0].ID != "chunk-1" {
		t.Fatalf("FetchUnextracted() = %#v", chunks)
	}

	if err := repo.MarkExtracted(context.Background(), ""); err == nil {
		t.Fatal("expected empty chunk id error")
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkExtracted(context.Background(), "chunk-1"); err != nil {
		t.Fatalf("MarkExtracted() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM project_memory_chunks")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	pending, err := repo.PendingCount(context.Background())
	if err != nil || pending != 9 {
		t.Fatalf("PendingCount() = %d, %v", pending, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FILTER")).
		WillReturnRows(sqlmock.NewRows([]string{"pending", "done"}).AddRow(2, 7))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM knowledge_entities")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM knowledge_edges")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM entity_mentions")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT type, count(*) FROM knowledge_entities GROUP BY type ORDER BY count(*) DESC")).
		WillReturnRows(sqlmock.NewRows([]string{"type", "count"}).AddRow("company", 2).AddRow("person", 1))
	stats, err := repo.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.ChunksPending != 2 || stats.ChunksDone != 7 || stats.EntitiesByType["company"] != 2 {
		t.Fatalf("Stats() = %#v", stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestChunkGraphExtractionRepository_ReflagChunksMissingEdges(t *testing.T) {
	t.Run("empty project id rejected", func(t *testing.T) {
		db, _, cleanup := newMockDBTX(t)
		defer cleanup()
		repo := NewChunkGraphExtractionRepository(db)
		if _, err := repo.ReflagChunksMissingEdges(context.Background(), "", false); err == nil {
			t.Fatal("expected error on empty project id")
		}
	})

	t.Run("countOnly issues SELECT count and skips UPDATE", func(t *testing.T) {
		db, mock, cleanup := newMockDBTX(t)
		defer cleanup()
		repo := NewChunkGraphExtractionRepository(db)

		mock.ExpectQuery(regexp.QuoteMeta("SELECT count(*) FROM (")).
			WithArgs("proj-a").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))

		n, err := repo.ReflagChunksMissingEdges(context.Background(), "proj-a", true)
		if err != nil {
			t.Fatalf("ReflagChunksMissingEdges(countOnly) error = %v", err)
		}
		if n != 42 {
			t.Fatalf("ReflagChunksMissingEdges(countOnly) = %d, want 42", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sql expectations: %v", err)
		}
	})

	t.Run("write path issues UPDATE and returns rows affected", func(t *testing.T) {
		db, mock, cleanup := newMockDBTX(t)
		defer cleanup()
		repo := NewChunkGraphExtractionRepository(db)

		mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
			WithArgs("proj-a").
			WillReturnResult(sqlmock.NewResult(0, 17))

		n, err := repo.ReflagChunksMissingEdges(context.Background(), "proj-a", false)
		if err != nil {
			t.Fatalf("ReflagChunksMissingEdges error = %v", err)
		}
		if n != 17 {
			t.Fatalf("ReflagChunksMissingEdges = %d, want 17", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet sql expectations: %v", err)
		}
	})
}

func TestIngestQueueRepositoryLifecycle(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewIngestQueueRepository(db)

	if err := repo.Enqueue(context.Background(), nil); err == nil {
		t.Fatal("expected nil item error")
	}
	if _, err := repo.ClaimBatch(context.Background(), "", 1); err == nil {
		t.Fatal("expected empty project id error")
	}

	class := "decision"
	execID := "exec-1"
	item := &persistence.IngestQueueItem{
		ProjectID: "proj-a", SourceArtifactID: "art-1", ProducerRole: "coder",
		IngestExecutionID: &execID, ProposedClass: &class, ProposedConfidence: 0.8,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_ingest_queue")).
		WithArgs(sqlmock.AnyArg(), item.ProjectID, item.SourceArtifactID, item.ProducerRole, item.IngestExecutionID, item.Priority, item.ProposedClass, item.ProposedConfidence, "queued", item.Attempts, sqlmock.AnyArg(), item.RepoScope).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Enqueue(context.Background(), item); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if item.ID == "" || item.State != "queued" || item.EnqueuedAt.IsZero() {
		t.Fatalf("Enqueue() did not set defaults: %#v", item)
	}

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("WITH claimed AS")).
		WithArgs("proj-a", 16).
		WillReturnRows(ingestQueueRows().
			AddRow("ing-2", "proj-a", "art-2", "tester", nil, int16(50), nil, float32(0), "processing", int16(1), now.Add(time.Minute), now, nil, nil, nil).
			AddRow("ing-1", "proj-a", "art-1", "coder", execID, int16(10), class, float32(0.8), "processing", int16(1), now, now, nil, nil, nil))
	claimed, err := repo.ClaimBatch(context.Background(), "proj-a", 0)
	if err != nil {
		t.Fatalf("ClaimBatch() error = %v", err)
	}
	if len(claimed) != 2 || claimed[0].ID != "ing-1" || claimed[1].ID != "ing-2" {
		t.Fatalf("ClaimBatch() ordering = %#v", claimed)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_ingest_queue")).
		WithArgs("ing-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkDone(context.Background(), "ing-1"); err != nil {
		t.Fatalf("MarkDone() error = %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("UPDATE project_ingest_queue")).
		WithArgs("ing-2", 3, "temporary").
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("queued"))
	terminal, err := repo.MarkFailed(context.Background(), "ing-2", 0, "temporary")
	if err != nil || terminal {
		t.Fatalf("MarkFailed retry = %v, %v", terminal, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE project_ingest_queue")).
		WithArgs("ing-3", 2, "terminal").
		WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("failed"))
	terminal, err = repo.MarkFailed(context.Background(), "ing-3", 2, "terminal")
	if err != nil || !terminal {
		t.Fatalf("MarkFailed terminal = %v, %v", terminal, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT project_id")).
		WithArgs(64).
		WillReturnRows(sqlmock.NewRows([]string{"project_id"}).AddRow("proj-a").AddRow("proj-b"))
	projects, err := repo.ProjectsWithQueued(context.Background(), 0)
	if err != nil || len(projects) != 2 {
		t.Fatalf("ProjectsWithQueued() = %#v, %v", projects, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)")).
		WithArgs("proj-a").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	depth, err := repo.QueueDepth(context.Background(), "proj-a")
	if err != nil || depth != 4 {
		t.Fatalf("QueueDepth() = %d, %v", depth, err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_ingest_queue")).
		WithArgs(int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	reset, err := repo.ResetStaleProcessing(context.Background(), -time.Second)
	if err != nil || reset != 3 {
		t.Fatalf("ResetStaleProcessing() = %d, %v", reset, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)")).
		WithArgs(int64(60)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	stale, err := repo.CountStaleProcessing(context.Background(), time.Minute)
	if err != nil || stale != 2 {
		t.Fatalf("CountStaleProcessing() = %d, %v", stale, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
