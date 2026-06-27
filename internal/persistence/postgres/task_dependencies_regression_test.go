package postgres

import (
	"context"
	"testing"
)

// TestGetDependencies_SelectsAllScanTaskColumns is the regression for
// the 2026-06-04 bug sweep: GetDependencies selected only 21 columns
// but scanTask reads 29, so every call against real Postgres failed
// with "expected 29 destination arguments, not 21". The eight trailing
// columns (brief_amended_at … chat_turn_id) were missing.
//
// sqlmock returns whatever rows we hand it (it won't reproduce the
// driver-side arity error), so this regression pins the *query text*:
// the SELECT must name the previously-missing columns. The expected
// pattern is matched as a regex against the actual SQL, so pre-fix —
// when dep.chat_turn_id / dep.open_checkpoint_id are absent — the query
// fails to match and the test fails.
func TestGetDependencies_SelectsAllScanTaskColumns(t *testing.T) {
	repo, mock, cleanup := newTaskRepo(t)
	defer cleanup()

	rows := taskRow().AddRow(fullTaskValues("dep-1", "p-1")...)
	// Require the eight columns that were missing pre-fix.
	mock.ExpectQuery(`dep\.brief_amended_at.*dep\.current_phase.*dep\.expected_by.*dep\.closed_at.*dep\.closed_by.*dep\.message_count.*dep\.open_checkpoint_id.*dep\.chat_turn_id`).
		WithArgs("task-1").
		WillReturnRows(rows)

	deps, err := repo.GetDependencies(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "dep-1" {
		t.Fatalf("GetDependencies returned %+v, want one task dep-1", deps)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
