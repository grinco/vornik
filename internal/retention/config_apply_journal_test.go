package retention

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// withinOf is a sqlmock argument matcher for a time bound within tolerance of
// want — the threshold is computed from time.Now inside the sweep.
type withinOf struct {
	want      time.Time
	tolerance time.Duration
}

func (w withinOf) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	if !ok {
		return false
	}
	d := got.Sub(w.want)
	if d < 0 {
		d = -d
	}
	return d <= w.tolerance
}

// expectGlobalSweepThroughRatings queues every always-on sweep that runs
// before the journal prune, all tables absent, so these tests assert only the
// journal step's own behaviour.
func expectGlobalSweepThroughRatings(mock sqlmock.Sqlmock) {
	for _, table := range []string{"ui_sessions", "api_keys", "link_codes", "execution_ratings"} {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public." + table + "')")).
			WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	}
}

// The journal is pruned on its OWN terminal transition, thirty days after it
// by default, and the DELETE must carry the terminal-only guard: a row in
// flight, or a DRIFT row parked for an operator, is never aged out (LLD
// 2026-09-13 config-apply-journal §1.5). The SQL text is the assertion here
// because the guard is a WHERE clause — nothing else stands between the
// sweeper and an open row's pre-images.
func TestSweepGlobal_PrunesOnlyTerminalJournalRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepThroughRatings(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.config_apply_journal')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(
		"DELETE FROM config_apply_journal WHERE terminal_at < $1 AND terminal_at IS NOT NULL AND state <> $2")).
		WithArgs(sqlmock.AnyArg(), "DRIFT").
		WillReturnResult(sqlmock.NewResult(0, 4))

	counts, err := s.SweepGlobal(context.Background(), GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if counts.ConfigApplyJournal != 4 {
		t.Fatalf("ConfigApplyJournal = %d, want 4", counts.ConfigApplyJournal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Zero means the design's default horizon, not "keep forever": the threshold
// bound to $1 must sit DefaultConfigApplyJournalDays back from now.
func TestSweepGlobal_JournalZeroDaysMeansTheDefaultHorizon(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepThroughRatings(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.config_apply_journal')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	want := time.Now().UTC().AddDate(0, 0, -DefaultConfigApplyJournalDays)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM config_apply_journal")).
		WithArgs(withinOf{want: want, tolerance: time.Minute}, "DRIFT").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := s.SweepGlobal(context.Background(), GlobalPolicy{ConfigApplyJournalDays: 0}); err != nil {
		t.Fatalf("SweepGlobal: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("zero days did not resolve to the default horizon: %v", err)
	}
}

// A deployment whose migration 184 has not landed must not take the whole
// retention cycle down — the same to_regclass guard the ratings sweep carries.
func TestSweepGlobal_TolerantOfAnAbsentJournalTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	s := New(db, zerolog.Nop())
	expectGlobalSweepThroughRatings(mock)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass('public.config_apply_journal')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	counts, err := s.SweepGlobal(context.Background(), GlobalPolicy{})
	if err != nil {
		t.Fatalf("SweepGlobal errored on an absent table: %v", err)
	}
	if counts.ConfigApplyJournal != 0 {
		t.Fatalf("ConfigApplyJournal = %d, want 0", counts.ConfigApplyJournal)
	}
}
