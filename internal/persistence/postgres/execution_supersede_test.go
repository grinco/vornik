package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestExecutionRepository_SupersedeNonTerminalForTask_HappyPath
// asserts the cascade query is shaped correctly and returns the
// affected-row count.
//
// Background: orphan PAUSED executions from the adaptive-route
// flow accumulated for days on one project until config reload
// noticed. The cascade is the proper-fix companion to the
// reload-safety-check interim fix.
func TestExecutionRepository_SupersedeNonTerminalForTask_HappyPath(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("task-orphans", "superseded_by_terminal_task", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := repo.SupersedeNonTerminalForTask(context.Background(), "task-orphans")
	if err != nil {
		t.Fatalf("SupersedeNonTerminalForTask: %v", err)
	}
	if n != 3 {
		t.Errorf("affected rows = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}

// TestExecutionRepository_SupersedeNonTerminalForTask_Idempotent
// asserts a no-op call (no non-terminal rows to sweep) returns
// zero rows without error. Important because the cascade fires
// on every task-terminal transition; the common case is no
// orphan present.
func TestExecutionRepository_SupersedeNonTerminalForTask_Idempotent(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE executions")).
		WithArgs("task-clean", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := repo.SupersedeNonTerminalForTask(context.Background(), "task-clean")
	if err != nil {
		t.Fatalf("SupersedeNonTerminalForTask: %v", err)
	}
	if n != 0 {
		t.Errorf("affected rows = %d, want 0 for the no-op case", n)
	}
}

// TestExecutionRepository_SupersedeNonTerminalForTask_QueryFiltersTerminal
// pins the WHERE clause shape — only non-terminal statuses are
// touched. Verifies the SQL contains the explicit terminal-list
// exclusion so a future edit that drops the filter (sweeping
// COMPLETED rows too) fails this test.
func TestExecutionRepository_SupersedeNonTerminalForTask_QueryFiltersTerminal(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	mock.ExpectExec(`status NOT IN \('COMPLETED', 'FAILED', 'CANCELLED'\)`).
		WithArgs("task-id", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := repo.SupersedeNonTerminalForTask(context.Background(), "task-id"); err != nil {
		t.Fatalf("SupersedeNonTerminalForTask: %v", err)
	}
}

// TestExecutionRepository_SupersedeStaleForTaskStart_StampsNewRunMarker pins the
// start-of-run sweep to its own audit marker. The two sweeps share SQL, so
// without this a refactor could silently label start-of-run orphans as
// terminal-cascade ones and destroy the distinction the error_code exists for.
//
// Regression for the 2026-07-14 "one paused task, three PAUSED badges" incident:
// resuming a parked task minted a fresh execution and stranded the parked row.
func TestExecutionRepository_SupersedeStaleForTaskStart_StampsNewRunMarker(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewExecutionRepository(db)

	mock.ExpectExec(`status NOT IN \('COMPLETED', 'FAILED', 'CANCELLED'\)`).
		WithArgs("task-resumed", "superseded_by_new_run", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := repo.SupersedeStaleForTaskStart(context.Background(), "task-resumed")
	if err != nil {
		t.Fatalf("SupersedeStaleForTaskStart: %v", err)
	}
	if n != 2 {
		t.Errorf("affected rows = %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations unmet: %v", err)
	}
}
