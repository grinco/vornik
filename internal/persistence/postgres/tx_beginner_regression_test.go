package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/persistence"
)

// newMetricsWrappedMockDB returns a *DBWithMetrics wrapping a sqlmock
// connection — i.e. exactly the handle the daemon injects in
// production (storage.Build(c.instrumentedDB())). The repo tests that
// use newMockDBTX exercise the raw *sql.DB path and therefore never
// caught the type-assertion bugs below.
func newMetricsWrappedMockDB(t *testing.T) (persistence.DBTX, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	wrapped := persistence.NewDBWithMetrics(db, persistence.NewDBMetrics(prometheus.NewRegistry()), "test")
	return wrapped, mock, func() { _ = db.Close() }
}

// TestRollbackTo_MetricsWrappedDB_DoesNotPanic is the regression for
// the 2026-06-04 bug sweep: CorpusEpochRepository.RollbackTo asserted
// r.db.(*sql.DB), which panics under the *DBWithMetrics wrapper the
// daemon actually injects, making operator-triggered corpus rollback
// unusable in production. Pre-fix this test panics; post-fix it drives
// the transaction cleanly.
func TestRollbackTo_MetricsWrappedDB_DoesNotPanic(t *testing.T) {
	db, mock, cleanup := newMetricsWrappedMockDB(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

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
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("p1", cut).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM corpus_epochs")).
		WithArgs("p1", cut).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("epoch-newer"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_rollbacks")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	deact, act, _, err := repo.RollbackTo(context.Background(), "p1", "epoch-1", "operator", "regression")
	if err != nil {
		t.Fatalf("RollbackTo under metrics wrapper: %v", err)
	}
	if deact != 2 || act != 3 {
		t.Errorf("deact/act = %d/%d, want 2/3", deact, act)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestRollbackTo_AlreadyInTx_RunsDirect covers the !ok branch added in
// the fix: when RollbackTo is handed an *sql.Tx, it runs its statements
// directly on it without nesting a transaction or committing (the outer
// caller owns commit/rollback).
func TestRollbackTo_AlreadyInTx_RunsDirect(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()

	mock.ExpectBegin()
	outerTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin outer tx: %v", err)
	}
	repo := NewCorpusEpochRepository(outerTx) // *sql.Tx -> BeginTx ok=false

	cut := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT created_at FROM corpus_epochs WHERE id = $1 AND project_id = $2")).
		WithArgs("epoch-1", "p1").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(cut))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM corpus_epochs_active")).
		WithArgs("p1", cut).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_epochs_active")).
		WithArgs("p1", cut, "operator", "regression").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks")).
		WithArgs("p1", cut).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM corpus_epochs")).
		WithArgs("p1", cut).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_rollbacks")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No ExpectCommit — RollbackTo must NOT commit the caller's tx.
	mock.ExpectCommit() // for the outer commit below

	if _, _, _, err := repo.RollbackTo(context.Background(), "p1", "epoch-1", "operator", "regression"); err != nil {
		t.Fatalf("RollbackTo in outer tx: %v", err)
	}
	if err := outerTx.Commit(); err != nil {
		t.Fatalf("commit outer tx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestRollbackTo_BeginError covers the begin-failure branch added with
// the persistence.BeginTx fix.
func TestRollbackTo_BeginError(t *testing.T) {
	db, mock, cleanup := newMetricsWrappedMockDB(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectBegin().WillReturnError(sql.ErrConnDone)
	if _, _, _, err := repo.RollbackTo(context.Background(), "p1", "epoch-1", "op", ""); err == nil {
		t.Fatal("begin failure must surface as an error")
	}
}

// TestRollbackTo_EmptyArgsRejected guards the input validation.
func TestRollbackTo_EmptyArgsRejected(t *testing.T) {
	db, _, cleanup := newMetricsWrappedMockDB(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)
	if _, _, _, err := repo.RollbackTo(context.Background(), "", "", "", ""); err == nil {
		t.Fatal("empty project/target must be rejected")
	}
}

// TestTaskMessageInsert_MetricsWrappedDB_IsTransactional is the
// regression for the sibling bug: TaskMessageRepository.Insert
// asserted r.db.(beginCtx) where beginCtx.BeginTx returns *sql.Tx.
// *DBWithMetrics.BeginTx returns *TxWithMetrics, so under the daemon's
// wrapper ok was always false and the INSERT + the tasks UPDATE ran as
// two un-transacted statements. This test demands a Begin/Commit pair;
// pre-fix the statements run on the pool with no Begin and
// ExpectationsWereMet fails.
func TestTaskMessageInsert_MetricsWrappedDB_IsTransactional(t *testing.T) {
	db, mock, cleanup := newMetricsWrappedMockDB(t)
	defer cleanup()
	repo := NewTaskMessageRepository(db)

	execID := "exec-1"
	authorID := "user-1"
	msg := &persistence.TaskMessage{
		TaskID: "task-1", ExecutionID: &execID, AuthorKind: "operator", AuthorID: &authorID,
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "need approval",
		Metadata: []byte(`{"resolved":false}`),
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_messages")).
		WithArgs(sqlmock.AnyArg(), msg.TaskID, msg.ExecutionID, msg.ParentID, msg.AuthorKind, msg.AuthorID, msg.MessageKind, msg.Content, `{"resolved":false}`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks")).
		WithArgs(sqlmock.AnyArg(), msg.TaskID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.Insert(context.Background(), msg); err != nil {
		t.Fatalf("Insert under metrics wrapper: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("Insert was not transactional under metrics wrapper: %v", err)
	}
}

// TestDeleteProjectData_MetricsWrappedDB_IsTransactional covers the
// third repo carrying the same type-assertion shape. Defense-in-depth:
// the archive sweeper wires this deleter with the raw pool today, but
// the assertion must still recognise the metrics wrapper.
func TestDeleteProjectData_MetricsWrappedDB_IsTransactional(t *testing.T) {
	db, mock, cleanup := newMetricsWrappedMockDB(t)
	defer cleanup()
	repo := NewProjectDataCleanupRepository(db)

	mock.ExpectBegin()
	for range persistence.ProjectDataTables {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ")).
			WithArgs("p1").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	stats, err := repo.DeleteProjectData(context.Background(), "p1")
	if err != nil {
		t.Fatalf("DeleteProjectData under metrics wrapper: %v", err)
	}
	if stats.TablesCleared != len(persistence.ProjectDataTables) {
		t.Errorf("TablesCleared = %d, want %d", stats.TablesCleared, len(persistence.ProjectDataTables))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("DeleteProjectData was not transactional under metrics wrapper: %v", err)
	}
}

// TestDeleteProjectData_EmptyProjectIDRejected guards the
// "blank ID would wipe every project" defensive check.
func TestDeleteProjectData_EmptyProjectIDRejected(t *testing.T) {
	db, _, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewProjectDataCleanupRepository(db)
	if _, err := repo.DeleteProjectData(context.Background(), ""); err == nil {
		t.Fatal("empty projectID must be rejected")
	}
}

// TestDeleteProjectData_AlreadyInTx_RunsDirect covers the non-tx
// fallback: when the repo is handed an *sql.Tx (an outer caller owns
// the transaction), the DELETEs run directly with no nested Begin.
func TestDeleteProjectData_AlreadyInTx_RunsDirect(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()

	mock.ExpectBegin()
	outerTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin outer tx: %v", err)
	}
	repo := NewProjectDataCleanupRepository(outerTx) // *sql.Tx -> BeginTx ok=false

	for range persistence.ProjectDataTables {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ")).
			WithArgs("p1").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	stats, err := repo.DeleteProjectData(context.Background(), "p1")
	if err != nil {
		t.Fatalf("DeleteProjectData in outer tx: %v", err)
	}
	if stats.TablesCleared != len(persistence.ProjectDataTables) {
		t.Errorf("TablesCleared = %d, want %d", stats.TablesCleared, len(persistence.ProjectDataTables))
	}
	if err := outerTx.Commit(); err != nil {
		t.Fatalf("commit outer tx: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestDeleteProjectData_RollsBackOnDeleteError exercises the
// transactional error path: a failing DELETE aborts the whole wipe.
func TestDeleteProjectData_RollsBackOnDeleteError(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t) // raw pool -> transactional path
	defer cleanup()
	repo := NewProjectDataCleanupRepository(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM ")).
		WithArgs("p1").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	if _, err := repo.DeleteProjectData(context.Background(), "p1"); err == nil {
		t.Fatal("a DELETE error must abort the wipe")
	}
}
