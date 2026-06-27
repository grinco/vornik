package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCountChildrenForParents_HappyPath covers the bulk-count query
// used by the UI list to render the "Subtasks (N)" pill without an
// N+1 GetChildren scan.
func TestCountChildrenForParents_HappyPath(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"parent_task_id", "count"}).
		AddRow("parent-a", 3).
		AddRow("parent-b", 1)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE parent_task_id = ANY($1)")).
		WillReturnRows(rows)

	got, err := repo.CountChildrenForParents(context.Background(), []string{"parent-a", "parent-b", "parent-c"})
	if err != nil {
		t.Fatalf("CountChildrenForParents: %v", err)
	}
	if got["parent-a"] != 3 || got["parent-b"] != 1 {
		t.Errorf("counts wrong: %+v", got)
	}
	// parent-c had no children — must be absent from the map, not zero-filled.
	if _, ok := got["parent-c"]; ok {
		t.Errorf("parent-c should be absent when it has no children, got %+v", got)
	}
}

// TestCountChildrenForParents_EmptyInput short-circuits without
// hitting the DB — important because passing an empty array to
// pq.Array would produce a SQL syntax error in older drivers.
func TestCountChildrenForParents_EmptyInput(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	got, err := repo.CountChildrenForParents(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
	// No SQL must have been issued.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}

// TestCountChildrenForParents_QueryError surfaces a DB error rather
// than returning a half-populated map — the caller's "best effort
// pill render" branch then decides what to do.
func TestCountChildrenForParents_QueryError(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE parent_task_id = ANY($1)")).
		WillReturnError(errors.New("boom"))
	_, err := repo.CountChildrenForParents(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected query error")
	}
}
