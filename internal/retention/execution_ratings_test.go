package retention

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// Ratings are pruned on their OWN horizon, not the execution's: a rating is
// small, it is the scarcest signal in the system, and phase 2 compares windows
// (design §6). The table is global — keyed (execution_id, rater_id), no
// project_id — so the sweep runs once per cycle beside the caches rather than
// inside the per-project loop.
func TestSweepGlobal_PrunesExecutionRatingsPastTheHorizon(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepPreamble(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.execution_ratings')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM execution_ratings WHERE created_at <")).
		WillReturnResult(sqlmock.NewResult(0, 3))

	counts, err := s.SweepGlobal(context.Background(), GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.ExecutionRatings != 3 {
		t.Fatalf("ExecutionRatings = %d, want 3", counts.ExecutionRatings)
	}
}

// Zero means the COMPILED DEFAULT here, not "disabled" — the opposite of the
// two cache knobs beside it, where zero means keep forever. The design chose a
// number rather than "forever" because an unbounded table is a decision nobody
// made, so an operator who sets nothing must still get a horizon.
func TestSweepGlobal_ZeroDaysMeansTheDefaultHorizonNotDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepPreamble(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.execution_ratings')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM execution_ratings WHERE created_at <")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := s.SweepGlobal(context.Background(), GlobalPolicy{ExecutionRatingsDays: 0}); err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	// The assertion is that the DELETE was ATTEMPTED at all. A zero read as
	// "disabled" would skip it and leave the expectation unmet.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("zero days skipped the prune instead of using the default: %v", err)
	}
}

// A deployment whose migration has not landed must not take the whole retention
// cycle down with it — the same to_regclass guard the caches carry.
func TestSweepGlobal_TolerantOfAnAbsentRatingsTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepPreamble(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.execution_ratings')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal errored on an absent table: %v", err)
	}
	if counts.ExecutionRatings != 0 {
		t.Fatalf("ExecutionRatings = %d, want 0", counts.ExecutionRatings)
	}
}

// expectGlobalSweepPreamble queues the always-on sweeps that run before the
// ratings prune, so these tests assert their own behaviour rather than the
// order of the ones already there.
func expectGlobalSweepPreamble(mock sqlmock.Sqlmock) {
	for _, table := range []string{"ui_sessions", "api_keys", "link_codes"} {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public." + table + "')")).
			WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM " + table)).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
}
