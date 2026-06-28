package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestExecutionRepository_Update — pin UPDATE shape + nil guard.
func TestExecutionRepository_Update(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	if err := repo.Update(context.Background(), nil); err == nil {
		t.Error("nil execution should error")
	}

	exec := &persistence.Execution{
		ID: "e1", ProjectID: "p", WorkflowID: "wf",
		WorkflowRevision: "v1", Status: persistence.ExecutionStatusRunning,
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Update(context.Background(), exec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if exec.UpdatedAt.IsZero() {
		t.Error("Update should bump UpdatedAt")
	}
}

// TestExecutionRepository_GetByTaskID — happy + ErrNotFound.
func TestExecutionRepository_GetByTaskID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	created := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, project_id, workflow_id")).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "project_id", "workflow_id", "workflow_revision",
			"status", "current_step_id", "completed_steps", "state_snapshot",
			"result", "error_message", "error_code", "started_at", "completed_at",
			"created_at", "updated_at",
			"parent_execution_id", "forked_from_step_id", "forked_prompt_override",
		}).AddRow("e1", "task-1", "p", "wf", "v1",
			"RUNNING", nil, "{}", nil,
			nil, nil, nil, nil, nil,
			created, created,
			nil, nil, nil))

	exec, err := repo.GetByTaskID(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetByTaskID: %v", err)
	}
	if exec.ID != "e1" {
		t.Errorf("ID = %q", exec.ID)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, task_id, project_id, workflow_id")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetByTaskID(context.Background(), "missing"); err != persistence.ErrNotFound {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}
}

// TestKnowledgeEntityRepository_GetByCanonical — covers Get path + guards.
func TestKnowledgeEntityRepository_GetByCanonical(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewKnowledgeEntityRepository(db)

	if _, err := repo.GetByCanonical(context.Background(), "", "", ""); err == nil {
		t.Error("empty args should error")
	}

	created := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, type, canonical_name")).
		WithArgs("p1", "PERSON", "alice").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "type", "canonical_name",
			"aliases", "description", "properties", "embedding",
			"extracted_by", "resolved_by", "confidence",
			"lifecycle_state", "validation_status", "epoch_id", "expires_at", "supersedes_id",
			"created_at", "updated_at",
		}).AddRow("e1", "p1", "PERSON", "alice",
			"[]", "", nil, nil,
			nil, nil, float32(0.9),
			"published", "unverified", nil, nil, nil,
			created, created))

	got, err := repo.GetByCanonical(context.Background(), "p1", "PERSON", "alice")
	if err != nil {
		t.Fatalf("GetByCanonical: %v", err)
	}
	if got.CanonicalName != "alice" {
		t.Errorf("name = %q", got.CanonicalName)
	}
}

// TestMemoryRetrievalAuditRepository_FeedbackStats — drives the 3-query
// FeedbackStats path and the UnretrievedChunkIDs path.
func TestMemoryRetrievalAuditRepository_FeedbackStats(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewMemoryRetrievalAuditRepository(db)

	since := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)\n\t\tFROM project_memory_chunks")).
		WithArgs("p1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(100))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)\n\t\tFROM memory_retrieval_audit")).
		WithArgs("p1", since).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(40))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(DISTINCT chunk_id)")).
		WithArgs("p1", since).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(30))

	stats, err := repo.FeedbackStats(context.Background(), "p1", since)
	if err != nil {
		t.Fatalf("FeedbackStats: %v", err)
	}
	if stats.TotalChunks != 100 || stats.TotalSearches != 40 || stats.RetrievedChunks != 30 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.UnretrievedChunks != 70 {
		t.Errorf("UnretrievedChunks = %d, want 70", stats.UnretrievedChunks)
	}

	// Guard.
	if _, err := repo.FeedbackStats(context.Background(), "", since); err == nil {
		t.Error("empty project should error")
	}
}

// TestMemoryRetrievalAuditRepository_FeedbackStats_NegativeClamp covers the
// defensive clamp when retrieved > total.
func TestMemoryRetrievalAuditRepository_FeedbackStats_NegativeClamp(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewMemoryRetrievalAuditRepository(db)
	since := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)\n\t\tFROM project_memory_chunks")).
		WithArgs("p1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)\n\t\tFROM memory_retrieval_audit")).
		WithArgs("p1", since).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(DISTINCT chunk_id)")).
		WithArgs("p1", since).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(7))
	stats, err := repo.FeedbackStats(context.Background(), "p1", since)
	if err != nil {
		t.Fatalf("FeedbackStats: %v", err)
	}
	if stats.UnretrievedChunks != 0 {
		t.Errorf("UnretrievedChunks should clamp to zero, got %d", stats.UnretrievedChunks)
	}
}

// TestMemoryRetrievalAuditRepository_UnretrievedChunkIDs.
func TestMemoryRetrievalAuditRepository_UnretrievedChunkIDs(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewMemoryRetrievalAuditRepository(db)

	since := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id\n\t\t\tFROM project_memory_chunks")).
		WithArgs("p1", since, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1").AddRow("c2"))

	ids, err := repo.UnretrievedChunkIDs(context.Background(), "p1", since, 0)
	if err != nil {
		t.Fatalf("UnretrievedChunkIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d", len(ids))
	}
	if _, err := repo.UnretrievedChunkIDs(context.Background(), "", since, 100); err == nil {
		t.Error("empty project should error")
	}
}

// TestTaskJudgeVerdictRepository_RecordAndQuery — Record happy path,
// duplicate detection, GetByTask happy + missing, ListRecent both
// project-scoped and global.
func TestTaskJudgeVerdictRepository_RecordAndQuery(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewTaskJudgeVerdictRepository(db)

	// nil guard.
	if err := repo.Record(context.Background(), nil); err == nil {
		t.Error("nil verdict should error")
	}

	v := &persistence.TaskJudgeVerdict{
		ID: "tv-1", ProjectID: "p1", TaskID: "t1", Role: "judge",
		Model: "m", Verdict: "pass", Confidence: 0.9, Summary: "ok",
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM task_judge_verdicts WHERE task_id = $1)")).
		WithArgs("t1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_judge_verdicts")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Record(context.Background(), v); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Duplicate detection.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM task_judge_verdicts WHERE task_id = $1)")).
		WithArgs("t1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if err := repo.Record(context.Background(), v); err != persistence.ErrDuplicateKey {
		t.Errorf("dup = %v, want ErrDuplicateKey", err)
	}

	// GetByTask happy.
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, role, model, verdict")).
		WithArgs("t1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "role", "model", "verdict",
			"confidence", "signals", "summary", "cost_usd", "recorded_at",
		}).AddRow("tv-1", "p1", "t1", "judge", "m", "pass",
			0.9, []byte(`{"s":1}`), "ok", 0.001, now))
	got, err := repo.GetByTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetByTask: %v", err)
	}
	if got.Verdict != "pass" {
		t.Errorf("verdict = %q", got.Verdict)
	}
	if len(got.Signals) == 0 {
		t.Error("Signals should be populated")
	}

	// GetByTask missing.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, role, model, verdict")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetByTask(context.Background(), "missing"); err != persistence.ErrNotFound {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}

	// ListRecent project-scoped + default limit.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, role, model, verdict")).
		WithArgs("p1", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "role", "model", "verdict",
			"confidence", "signals", "summary", "cost_usd", "recorded_at",
		}).AddRow("tv-1", "p1", "t1", "judge", "m", "pass",
			0.9, []byte("{}"), "ok", 0.001, now))
	out, err := repo.ListRecent(context.Background(), "p1", 0)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("len = %d", len(out))
	}

	// ListRecent global.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, task_id, role, model, verdict")).
		WithArgs(5).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "role", "model", "verdict",
			"confidence", "signals", "summary", "cost_usd", "recorded_at",
		}))
	if _, err := repo.ListRecent(context.Background(), "", 5); err != nil {
		t.Fatalf("ListRecent global: %v", err)
	}
}

// TestCorpusEpochRepository_RollbackTo — RollbackTo happy + guards.
// Drives the multi-step transaction.
func TestCorpusEpochRepository_RollbackTo(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	// Guards.
	if _, _, _, err := repo.RollbackTo(context.Background(), "", "", "", ""); err == nil {
		t.Error("empty args should error")
	}

	cut := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT created_at FROM corpus_epochs WHERE id = $1 AND project_id = $2")).
		WithArgs("epoch-1", "p1").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(cut))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM corpus_epochs_active")).
		WithArgs("p1", cut).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_epochs_active")).
		WithArgs("p1", cut, "operator", "regression").
		WillReturnResult(sqlmock.NewResult(0, 3))
	// Restore pass (migration 89): un-supersede chunks whose causing
	// epoch was just deactivated.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("p1", cut).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM corpus_epochs")).
		WithArgs("p1", cut).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("epoch-newer"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_rollbacks")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	deact, act, restored, err := repo.RollbackTo(context.Background(), "p1", "epoch-1", "operator", "regression")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if deact != 2 || act != 3 || restored != 4 {
		t.Errorf("deact/act/restored = %d/%d/%d, want 2/3/4", deact, act, restored)
	}
}

// TestCorpusEpochRepository_RollbackTo_TargetNotFound covers the
// scan-error branch in RollbackTo.
func TestCorpusEpochRepository_RollbackTo_TargetNotFound(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT created_at FROM corpus_epochs WHERE id = $1 AND project_id = $2")).
		WithArgs("missing", "p1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	if _, _, _, err := repo.RollbackTo(context.Background(), "p1", "missing", "op", ""); err == nil {
		t.Error("expected error for missing target")
	}
}

// TestCorpusEpochRepository_ListRollbacks covers the listing query.
func TestCorpusEpochRepository_ListRollbacks(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	if _, err := repo.ListRollbacks(context.Background(), "", 0); err == nil {
		t.Error("empty project should error")
	}

	applied := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, project_id, from_epoch_id, to_epoch_id, triggered_by, reason, applied_at")).
		WithArgs("p1", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "from_epoch_id", "to_epoch_id",
			"triggered_by", "reason", "applied_at", "chunks_restored",
		}).AddRow("rb-1", "p1", "epoch-newer", "epoch-1", "operator", "regression", applied, 2))

	out, err := repo.ListRollbacks(context.Background(), "p1", 0)
	if err != nil {
		t.Fatalf("ListRollbacks: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("len = %d", len(out))
	}
}

// TestDB_StatsAndClose — exercises the wrappers using a sqlmock-backed
// *sql.DB so the methods run without a real Postgres.
func TestDB_StatsAndClose(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	d := &DB{DB: mockDB}
	if s := d.Stats(); s == nil {
		t.Error("Stats should return non-nil")
	}
	mock.ExpectClose()
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestDB_MigrateAndRunner — covers MigrationRunner + Migrate happy paths
// with sqlmock simulating an empty migrations table.
func TestDB_MigrateAndRunner(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = mockDB.Close() }()
	d := &DB{DB: mockDB}

	runner := d.MigrationRunner()
	if runner == nil {
		t.Fatal("MigrationRunner returned nil")
	}

	// Migrate runs the full chain: take pg_advisory_lock,
	// CREATE migrations, sync bootstrap, read the full applied-version set,
	// then apply each pending migration, finally release the
	// advisory lock via defer. Sqlmock the smallest path:
	// bootstrap already applied + every version already recorded so
	// nothing needs to apply.
	//
	// Magic value 0x73776D646D696772 mirrors persistence.
	// migrationLockKey — kept in sync deliberately, since the
	// postgres package can't import the unexported helper.
	const migLockKey int64 = 0x73776D646D696772
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_lock($1)")).
		WithArgs(migLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	appliedRows := sqlmock.NewRows([]string{"version"})
	hasMemoryHardening := false
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 23 {
			hasMemoryHardening = true
		}
		appliedRows.AddRow(m.Version)
	}
	if !hasMemoryHardening {
		t.Fatal("DefaultMigrations must include v23 memory hardening migration")
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT version FROM migrations")).
		WillReturnRows(appliedRows)
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_unlock($1)")).
		WithArgs(migLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

// TestDB_IsReady_PingFails — the ping-fail branch.
func TestDB_IsReady_PingFails(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = mockDB.Close() }()
	d := &DB{DB: mockDB}

	mock.ExpectPing().WillReturnError(errors.New("ping boom"))
	if err := d.IsReady(context.Background()); err == nil {
		t.Error("expected IsReady to fail on ping error")
	}
}

// TestExecutionStepOutcomeRepository_FinalizePending — happy + not-found
// branches exercising the UPDATE ... RETURNING shape.
func TestExecutionStepOutcomeRepository_FinalizePending(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionStepOutcomeRepository(db)

	attrID := "step-a"
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("ok", "", "", attrID, "e1", "s1").
		WillReturnRows(sqlmock.NewRows([]string{"role", "model"}).AddRow("worker", "claude"))
	role, model, err := repo.FinalizePending(context.Background(), "e1", "s1", "ok", "", "", &attrID)
	if err != nil {
		t.Fatalf("FinalizePending: %v", err)
	}
	if role != "worker" || model != "claude" {
		t.Errorf("role/model = %q/%q", role, model)
	}

	// Not found.
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE execution_step_outcomes")).
		WithArgs("ok", "", "", nil, "e2", "s2").
		WillReturnError(sql.ErrNoRows)
	if _, _, err := repo.FinalizePending(context.Background(), "e2", "s2", "ok", "", "", nil); err != persistence.ErrNotFound {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}
}
