package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Statement-shape pins for the rollback × supersession fix (migration
// 89) on the postgres backend. The full behavioural semantics are
// pinned by the real-database suite in
// internal/persistence/sqlite/rollback_supersession_regression_test.go;
// these tests pin the postgres SQL text so the two backends cannot
// silently diverge. All three fail against the pre-fix statements.

// TestRollbackTo_RestorePassAndTombstoneShape — the re-activation
// INSERT must exclude tombstoned epochs, and the restore UPDATE must
// put back the prior validation_status and clear the provenance.
func TestRollbackTo_RestorePassAndTombstoneShape(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	cut := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT created_at FROM corpus_epochs WHERE id = $1 AND project_id = $2")).
		WithArgs("epoch-1", "p1").
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(cut))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM corpus_epochs_active")).
		WithArgs("p1", cut).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Tombstone exclusion: pre-fix the INSERT lacked the
	// deactivated_at clause and resurrected explicitly-deactivated
	// epochs.
	mock.ExpectExec(`(?s)INSERT INTO corpus_epochs_active.*closed_at IS NOT NULL.*deactivated_at IS NULL`).
		WithArgs("p1", cut, "operator", "r").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Restore pass: pre-fix this statement did not exist at all.
	mock.ExpectExec(`(?s)UPDATE project_memory_chunks.*SET validation_status\s+= COALESCE\(NULLIF\(pre_supersede_status, ''\), 'unverified'\).*superseded_in_epoch\s+= NULL.*pre_supersede_status = NULL.*validation_status = 'superseded'.*superseded_in_epoch IN`).
		WithArgs("p1", cut).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM corpus_epochs")).
		WithArgs("p1", cut).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// Audit row carries the restore count.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_rollbacks")).
		WithArgs(sqlmock.AnyArg(), "p1", nil, "epoch-1", "operator", "r", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	_, _, restored, err := repo.RollbackTo(context.Background(), "p1", "epoch-1", "operator", "r")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if restored != 3 {
		t.Errorf("chunksRestored = %d, want 3", restored)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("statement shapes drifted: %v", err)
	}
}

// TestCountRollbackRestorable_ShapeAndGuard — preview counts split
// restorable (causing epoch would deactivate) from non-restorable
// (NULL provenance), plus the empty-args guard.
func TestCountRollbackRestorable_ShapeAndGuard(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	if _, _, err := repo.CountRollbackRestorable(context.Background(), "", ""); err == nil {
		t.Error("empty args must error")
	}

	mock.ExpectQuery(`(?s)SELECT.*FILTER \(WHERE c\.superseded_in_epoch IN.*FILTER \(WHERE c\.superseded_in_epoch IS NULL\).*validation_status = 'superseded'`).
		WithArgs("p1", "epoch-1").
		WillReturnRows(sqlmock.NewRows([]string{"restorable", "non_restorable"}).AddRow(4, 2))

	restorable, nonRestorable, err := repo.CountRollbackRestorable(context.Background(), "p1", "epoch-1")
	if err != nil {
		t.Fatalf("CountRollbackRestorable: %v", err)
	}
	if restorable != 4 || nonRestorable != 2 {
		t.Errorf("counts = (%d,%d), want (4,2)", restorable, nonRestorable)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestDeactivate_StampsTombstone_ActivateClears — the tombstone
// round-trip on the postgres statements.
func TestDeactivate_StampsTombstone_ActivateClears(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewCorpusEpochRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM corpus_epochs_active")).
		WithArgs("p1", "e1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE corpus_epochs.*SET deactivated_at = NOW\(\), deactivated_by = \$3.*deactivated_at IS NULL`).
		WithArgs("e1", "p1", "operator").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Deactivate(context.Background(), "p1", "e1", "operator"); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO corpus_epochs_active")).
		WithArgs("p1", "e1", "operator", "back").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE corpus_epochs.*SET deactivated_at = NULL, deactivated_by = NULL.*deactivated_at IS NOT NULL`).
		WithArgs("e1", "p1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Activate(context.Background(), "p1", "e1", "operator", "back"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
