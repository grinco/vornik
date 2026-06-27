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
		WithArgs("task-orphans").
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
		WithArgs("task-clean").
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
		WithArgs("task-id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := repo.SupersedeNonTerminalForTask(context.Background(), "task-id"); err != nil {
		t.Fatalf("SupersedeNonTerminalForTask: %v", err)
	}
}
