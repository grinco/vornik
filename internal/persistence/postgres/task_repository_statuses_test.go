package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/persistence"
)

// TestTaskList_Statuses_IssuesStatusAnyClause — Statuses non-empty emits
// "status = ANY($n)" (the ProjectIDs-style IN(...) widening, Outcome Inbox
// design §5.2), not the single-status "status = $n" branch.
func TestTaskList_Statuses_IssuesStatusAnyClause(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("status = ANY($")).
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{
		Statuses: []persistence.TaskStatus{
			persistence.TaskStatusAwaitingApproval,
			persistence.TaskStatusFailed,
		},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected status = ANY($..) query, unmet: %v", err)
	}
}

// TestTaskList_StatusOnly_StillUsesEqualityClause pins the legacy
// single-status callers: Statuses unset must keep emitting "status = $n",
// never the ANY() form.
func TestTaskList_StatusOnly_StillUsesEqualityClause(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	status := persistence.TaskStatusQueued
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks")).
		WithArgs(status).
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{Status: &status})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

// TestTaskList_BothSet_StatusesWins — Statuses and Status both set:
// the query must be built from Statuses (ANY clause), and Status's value
// must not appear as a bind arg at all.
func TestTaskList_BothSet_StatusesWins(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	legacy := persistence.TaskStatusCompleted
	mock.ExpectQuery(regexp.QuoteMeta("status = ANY($")).
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{
		Status:   &legacy,
		Statuses: []persistence.TaskStatus{persistence.TaskStatusFailed},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Statuses should have won (ANY clause expected): %v", err)
	}
}

// TestTaskList_EmptyStatuses_FallsThroughToStatus — an empty non-nil
// Statuses must NOT emit "status = ANY($)" (which would be an empty
// IN() footgun); with Status also set it falls back to the equality
// clause, and with neither set it emits no status predicate at all.
func TestTaskList_EmptyStatuses_FallsThroughToStatus(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	status := persistence.TaskStatusRunning
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks")).
		WithArgs(status).
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{
		Status:   &status,
		Statuses: []persistence.TaskStatus{},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestTaskList_EmptyStatuses_NoStatusAtAll_ReturnsAll(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks WHERE 1=1")).
		WithArgs().
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{
		Statuses: []persistence.TaskStatus{},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

// TestTaskCount_Statuses mirrors the List coverage for Count, since the
// brief calls out updating both in lockstep.
func TestTaskCount_Statuses(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("status = ANY($")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))

	got, err := repo.Count(context.Background(), persistence.TaskFilter{
		Statuses: []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusAwaitingApproval},
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 2 {
		t.Errorf("Count = %d, want 2", got)
	}
}

// TestTaskList_IDs_IssuesIDInClause — the batch by-ID filter added for
// persistence.ResolveRequestRoots (Outcome Inbox design §5.3).
func TestTaskList_IDs_IssuesIDInClause(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("AND id IN (")).
		WillReturnRows(taskRow())

	_, err := repo.List(context.Background(), persistence.TaskFilter{IDs: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected an id IN (...) clause: %v", err)
	}
}
