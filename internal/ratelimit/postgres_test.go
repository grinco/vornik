package ratelimit

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestPostgresProjectLimiter_NoCapsDisabled — caps of zero on
// the project skip the DB call entirely. Mirrors the in-process
// limiter's contract so callers can opt in per project.
func TestPostgresProjectLimiter_NoCapsDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// No queries should fire — the assertion is that mock.ExpectationsWereMet
	// passes despite us calling Check.

	l := NewPostgresProjectLimiter(db)
	d := l.Check(&registry.Project{ID: "p1"}, time.Now())
	assert.False(t, d.Blocked)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_CheckUnderLimit — both aggregates
// return values below the configured caps so the call is allowed.
func TestPostgresProjectLimiter_CheckUnderLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(15))

	l := NewPostgresProjectLimiter(db)
	p := &registry.Project{ID: "p1", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 5, TasksPerHour: 50}}
	d := l.Check(p, time.Now())
	assert.False(t, d.Blocked)
	assert.Equal(t, 2, d.MinuteCount)
	assert.Equal(t, 15, d.HourCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_CheckMinuteCapReached — the
// trailing-minute aggregate returns the cap; Decision.Blocked
// is true with the minute-cap reason. We DON'T expect a second
// query because the implementation aggregates both windows up
// front before the cap comparison, but the order of the queries
// matters for sqlmock; both are queued.
func TestPostgresProjectLimiter_CheckMinuteCapReached(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))

	l := NewPostgresProjectLimiter(db)
	p := &registry.Project{ID: "p1", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 5, TasksPerHour: 50}}
	d := l.Check(p, time.Now())
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "minute")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_CheckHourCapReached — minute is
// under but hour is at cap; block reason is hour.
func TestPostgresProjectLimiter_CheckHourCapReached(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(50))

	l := NewPostgresProjectLimiter(db)
	p := &registry.Project{ID: "p1", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 5, TasksPerHour: 50}}
	d := l.Check(p, time.Now())
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "hour")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_FailsOpenOnDBError — a DB failure
// returns the zero Decision so the daemon doesn't black-hole
// task creation on a transient hiccup. Caps are defensive; an
// over-allowance is acceptable when the alternative is refusing
// every request.
func TestPostgresProjectLimiter_FailsOpenOnDBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WillReturnError(errors.New("db down"))

	l := NewPostgresProjectLimiter(db)
	p := &registry.Project{ID: "p1", RateLimit: registry.ProjectRateLimit{TasksPerMinute: 5}}
	d := l.Check(p, time.Now())
	assert.False(t, d.Blocked, "DB error must fail-open, not block")
}

// TestPostgresProjectLimiter_RecordUpsertsBothWindows — Record
// emits two UPSERTs (minute + hour), each keyed on the truncated
// bucket boundary. Verify both fire in order with the correct
// scope_kind + scope_key.
func TestPostgresProjectLimiter_RecordUpsertsBothWindows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(upsertCounterSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertCounterSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	l := NewPostgresProjectLimiter(db)
	l.Record("p1", time.Now())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_MinuteAndHourBucketsDoNotConflate is the
// regression guard for the 2026-06-25 rate-limit double-count bug. Record
// wrote the minute AND hour buckets under the same scope_kind, so the count
// query — which aggregates by (scope_kind, scope_key, window_start range) with
// no granularity column — cross-summed the two granularities: (a) the hour
// aggregate counted every event twice (the hour cap tripped at HALF the
// configured rate), and (b) during minute 0 of each hour the minute bucket and
// hour bucket truncate to the same window_start, colliding into one
// double-incremented row (the horizontal-scaling integration flake). The fix:
// the two granularities use distinct scope_kind.
func TestPostgresProjectLimiter_MinuteAndHourBucketsDoNotConflate(t *testing.T) {
	require.NotEqual(t, scopeProject, scopeProjectHour,
		"minute and hour granularities must use distinct scope_kind so they cannot collide or cross-sum")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Record writes the minute bucket under scopeProject and the hour bucket
	// under scopeProjectHour — never the same kind.
	mock.ExpectExec(regexp.QuoteMeta(upsertCounterSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertCounterSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	l := NewPostgresProjectLimiter(db)
	l.Record("p1", time.Now())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_RecordNilSafe — nil receiver +
// empty project ID short-circuit without touching the DB.
func TestPostgresProjectLimiter_RecordNilSafe(t *testing.T) {
	var l *PostgresProjectLimiter
	l.Record("p1", time.Now()) // must not panic
	l.RecordCtx(context.Background(), "", time.Now())

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	live := NewPostgresProjectLimiter(db)
	live.Record("", time.Now()) // empty key skips the DB call
}

// TestPostgresProjectLimiter_SweepExpiredDeletesOlderRows —
// SweepExpired emits the DELETE with the cutoff timestamp and
// returns the rows-affected count for operator logs.
func TestPostgresProjectLimiter_SweepExpiredDeletesOlderRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(sweepCounterSQL)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 42))

	l := NewPostgresProjectLimiter(db)
	n, err := l.SweepExpired(context.Background(), 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(42), n)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_SweepExpiredZeroRetentionNoOp —
// negative or zero retention skips the DELETE; the janitor
// loop interprets zero as "feature disabled".
func TestPostgresProjectLimiter_SweepExpiredZeroRetentionNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	l := NewPostgresProjectLimiter(db)
	n, err := l.SweepExpired(context.Background(), 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_CheckNilSafe — nil receiver and
// nil project return the zero Decision rather than panicking.
func TestPostgresProjectLimiter_CheckNilSafe(t *testing.T) {
	var l *PostgresProjectLimiter
	assert.Equal(t, Decision{}, l.Check(nil, time.Now()))

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	live := NewPostgresProjectLimiter(db)
	assert.Equal(t, Decision{}, live.Check(nil, time.Now()))
}

// TestPostgresProjectLimiter_SatisfiesInterfaces — compile-time
// check that the type satisfies both ProjectLimiter and the
// context-aware variant. Documented in the package so callers
// can swap backends behind the interface.
func TestPostgresProjectLimiter_SatisfiesInterfaces(t *testing.T) {
	var _ ProjectLimiter = (*PostgresProjectLimiter)(nil)
	var _ ProjectLimiterCtx = (*PostgresProjectLimiter)(nil)
}

// TestPostgresProjectLimiter_HourAggregateError_FailsOpen — covers
// the second of countWindows' two QueryRow calls. The first
// (minute) aggregate succeeds and the second (hour) errors; the
// limiter must still fail open so the rest of the daemon doesn't
// stall on a per-project caching glitch. Without this test the
// hour-error branch sits at the "covered by happy path" baseline
// only — flipping a future regression silently.
func TestPostgresProjectLimiter_HourAggregateError_FailsOpen(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProject, "p1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(countSumSQL)).
		WithArgs(scopeProjectHour, "p1", sqlmock.AnyArg()).
		WillReturnError(errors.New("hour aggregate boom"))

	l := NewPostgresProjectLimiter(db)
	p := &registry.Project{ID: "p1", RateLimit: registry.ProjectRateLimit{TasksPerHour: 5}}
	d := l.Check(p, time.Now())
	assert.False(t, d.Blocked, "DB error on hour aggregate must fail-open")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresProjectLimiter_SweepExpiredDBError — the janitor's
// DELETE can fail (table lock, transient DB hiccup). The error must
// propagate so the operator sees it in the janitor log, rather than
// silently reporting "0 rows swept".
func TestPostgresProjectLimiter_SweepExpiredDBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectExec(regexp.QuoteMeta(sweepCounterSQL)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errors.New("table locked"))

	l := NewPostgresProjectLimiter(db)
	n, err := l.SweepExpired(context.Background(), time.Hour)
	if err == nil {
		t.Fatal("expected an error from SweepExpired on Exec failure, got nil")
	}
	if n != 0 {
		t.Errorf("rows = %d on Exec failure, want 0", n)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}
