package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestOrphanChildren_HappyPath — the retry-path child-detach issues a
// single UPDATE that NULLs parent_task_id for every direct child and
// returns the affected count. Backlog item 1 (task-retry fast-fail storm).
func TestOrphanChildren_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET parent_task_id = NULL")).
		WithArgs("parent-a").
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := repo.OrphanChildren(context.Background(), "parent-a")
	if err != nil {
		t.Fatalf("OrphanChildren: %v", err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestOrphanChildren_QueryError surfaces the DB error rather than a
// bogus zero count, so the caller logs it and proceeds (best-effort).
func TestOrphanChildren_QueryError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("UPDATE tasks SET parent_task_id = NULL")).
		WithArgs("parent-a").
		WillReturnError(errors.New("boom"))

	if _, err := repo.OrphanChildren(context.Background(), "parent-a"); err == nil {
		t.Fatal("expected query error")
	}
}
