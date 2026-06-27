package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

func newFillRepo(t *testing.T) (*TradingFillRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTradingFillRepository(db), mock, func() { _ = db.Close() }
}

// ---------- Record ----------

func TestFillRecord_Validation(t *testing.T) {
	repo, _, cleanup := newFillRepo(t)
	defer cleanup()

	cases := []struct {
		name   string
		fill   *persistence.TradingFill
		errSub string
	}{
		{"nil", nil, "nil trading fill"},
		{"empty id", &persistence.TradingFill{OrderID: "o", ProjectID: "p", Symbol: "S"}, "fill ID required"},
		{"empty order id", &persistence.TradingFill{ID: "f", ProjectID: "p", Symbol: "S"}, "order ID required"},
		{"empty project id", &persistence.TradingFill{ID: "f", OrderID: "o", Symbol: "S"}, "project ID required"},
		{"empty symbol", &persistence.TradingFill{ID: "f", OrderID: "o", ProjectID: "p"}, "symbol required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := repo.Record(context.Background(), tc.fill)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("expected %q, got %v", tc.errSub, err)
			}
		})
	}
}

func TestFillRecord_HappyPath_WithCommission(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	filled := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	commission := 0.75
	fill := &persistence.TradingFill{
		ID:            "f-1",
		OrderID:       "ord-1",
		ProjectID:     "p-1",
		Symbol:        "AAPL",
		Qty:           10.0,
		Price:         150.25,
		CommissionUSD: &commission,
		FilledAt:      filled,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_fills")).
		WithArgs(
			"f-1", "ord-1", "p-1", "AAPL",
			10.0, 150.25, 0.75, filled,
			nil, nil, "reconcile", nil, // exec_id, account_id, source, source_detail
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), fill); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestFillRecord_NilCommissionBindsAsNull pins the
// ptrFloatOrNil contract: nil pointer -> SQL NULL. Without this,
// a fill from a broker that doesn't report commission would write
// `0` and the soak-panel commission tile would understate spend.
func TestFillRecord_NilCommissionBindsAsNull(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	filled := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	fill := &persistence.TradingFill{
		ID:        "f-2",
		OrderID:   "ord-1",
		ProjectID: "p-1",
		Symbol:    "AAPL",
		Qty:       1.0,
		Price:     100.0,
		// CommissionUSD nil
		FilledAt: filled,
	}

	mock.ExpectExec("INSERT INTO trading_fills").
		WithArgs(
			"f-2", "ord-1", "p-1", "AAPL",
			1.0, 100.0,
			nil, // commission -> NULL
			filled,
			nil, nil, "reconcile", nil, // exec_id, account_id, source, source_detail
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), fill); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestFillRecord_DefaultsFilledAt covers the zero-time defaulting
// path. The broker's poll loop sometimes posts fills with
// FilledAt unset when the upstream report didn't carry a timestamp.
func TestFillRecord_DefaultsFilledAt(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	fill := &persistence.TradingFill{
		ID:        "f-3",
		OrderID:   "ord-1",
		ProjectID: "p-1",
		Symbol:    "AAPL",
		Qty:       1.0,
		Price:     100.0,
	}

	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectExec("INSERT INTO trading_fills").
		WithArgs(
			"f-3", "ord-1", "p-1", "AAPL",
			1.0, 100.0, nil,
			recentTimeMatcher{notBefore: before},
			nil, nil, "reconcile", nil, // exec_id, account_id, source, source_detail
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), fill); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestFillRecord_ConflictDoNothingIsSilent: the broker's retry
// path posts the same (order_id, filled_at)-derived ID across
// outage recoveries. ON CONFLICT (id) DO NOTHING is the contract
// that keeps fill volume from being double-counted.
func TestFillRecord_ConflictDoNothingIsSilent(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (id) DO NOTHING")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := repo.Record(context.Background(), &persistence.TradingFill{
		ID: "dup", OrderID: "o", ProjectID: "p", Symbol: "S",
		Qty: 1, Price: 1, FilledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("dup retry should be silent, got %v", err)
	}
}

func TestFillRecord_PropagatesDBError(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO trading_fills").
		WillReturnError(errors.New("conn closed"))

	err := repo.Record(context.Background(), &persistence.TradingFill{
		ID: "f", OrderID: "o", ProjectID: "p", Symbol: "S",
		Qty: 1, Price: 1, FilledAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------- List ----------

func fillRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "order_id", "project_id", "symbol",
		"qty", "price", "commission_usd", "filled_at",
	})
}

// TestFillList_DefaultPageSize asserts PageSize=0 -> 100. Without
// this the soak panel "recent fills" would return 0 rows by default.
func TestFillList_DefaultPageSize(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	filled := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	commission := 0.5
	rows := fillRows().
		AddRow("f-1", "ord-1", "p-1", "AAPL", 10.0, 150.0, commission, filled).
		AddRow("f-2", "ord-2", "p-1", "MSFT", 5.0, 200.0, nil, filled)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_fills WHERE 1=1")).
		WithArgs(100).
		WillReturnRows(rows)

	out, err := repo.List(context.Background(), persistence.TradingFillFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].CommissionUSD == nil || *out[0].CommissionUSD != 0.5 {
		t.Errorf("row 0 commission roundtrip: %v", out[0].CommissionUSD)
	}
	if out[1].CommissionUSD != nil {
		t.Errorf("row 1 commission should be nil, got %v", *out[1].CommissionUSD)
	}
}

// TestFillList_CapsPageSize: caller passing PageSize=10000 must be
// clamped to 5000 (full-table-scan guard).
func TestFillList_CapsPageSize(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_fills").
		WithArgs(5000).
		WillReturnRows(fillRows())

	if _, err := repo.List(context.Background(), persistence.TradingFillFilter{
		PageSize: 10000,
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestFillList_AllFilters(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	pid, oid, sym := "p-1", "ord-1", "AAPL"
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_fills")).
		WithArgs("p-1", "ord-1", "AAPL", since, until, 50, 5).
		WillReturnRows(fillRows())

	_, err := repo.List(context.Background(), persistence.TradingFillFilter{
		ProjectID: &pid, OrderID: &oid, Symbol: &sym,
		Since: &since, Until: &until,
		PageSize: 50, Offset: 5,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestFillList_QueryError(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectQuery("FROM trading_fills").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.List(context.Background(), persistence.TradingFillFilter{}); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- SumVolume ----------

// TestSumVolume_NoFilters: cross-everything volume sum, COALESCE
// for the empty-rows case. This is the realised-volume figure the
// soak panel reads to detect runaway trading.
func TestSumVolume_NoFilters(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(qty * price), 0) FROM trading_fills WHERE 1=1")).
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(15000.0))

	got, err := repo.SumVolume(context.Background(), persistence.TradingFillFilter{})
	if err != nil {
		t.Fatalf("SumVolume: %v", err)
	}
	if got != 15000.0 {
		t.Errorf("expected 15000, got %v", got)
	}
}

func TestSumVolume_AllFilters(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	pid, oid, sym := "p-1", "ord-1", "AAPL"
	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery("SUM\\(qty \\* price\\)").
		WithArgs("p-1", "ord-1", "AAPL", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(1500.0))

	got, err := repo.SumVolume(context.Background(), persistence.TradingFillFilter{
		ProjectID: &pid, OrderID: &oid, Symbol: &sym,
		Since: &since, Until: &until,
	})
	if err != nil {
		t.Fatalf("SumVolume: %v", err)
	}
	if got != 1500.0 {
		t.Errorf("expected 1500, got %v", got)
	}
}

// TestSumVolume_IgnoresPageSizeAndOffset: SumVolume is an
// aggregate over the full match set; PageSize/Offset on the filter
// must NOT show up as $N args. A regression that started honoring
// them would silently undercount realised volume.
func TestSumVolume_IgnoresPageSizeAndOffset(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	pid := "p-1"
	mock.ExpectQuery("FROM trading_fills WHERE 1=1").
		WithArgs("p-1"). // ONLY the project filter, no LIMIT/OFFSET args
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(42.0))

	_, err := repo.SumVolume(context.Background(), persistence.TradingFillFilter{
		ProjectID: &pid,
		PageSize:  10, Offset: 5, // both should be ignored
	})
	if err != nil {
		t.Fatalf("SumVolume: %v", err)
	}
}

func TestSumVolume_QueryError(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectQuery("SUM").
		WillReturnError(errors.New("conn closed"))

	if _, err := repo.SumVolume(context.Background(), persistence.TradingFillFilter{}); err == nil {
		t.Fatal("expected error")
	}
}

// ---------- exec-column persistence (Task 3) ----------

// TestFillRecord_WritesExecColumns pins that the four new exec-
// keyed columns (exec_id, account_id, source, source_detail) are
// bound as $9–$12 in the INSERT statement.
func TestFillRecord_WritesExecColumns(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()
	exec, acct, detail := "ex-1", "DUH1", "perm=2001"
	fill := &persistence.TradingFill{
		ID: "exec-ex-1", OrderID: "ord-1", ProjectID: "p-1", Symbol: "AAPL",
		Qty: 4, Price: 338.75, ExecID: &exec, AccountID: &acct,
		Source: "reconcile", SourceDetail: &detail,
		FilledAt: time.Date(2026, 6, 25, 16, 8, 54, 0, time.UTC),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_fills")).
		WithArgs("exec-ex-1", "ord-1", "p-1", "AAPL", 4.0, 338.75, nil,
			fill.FilledAt, "ex-1", "DUH1", "reconcile", "perm=2001").
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.Record(context.Background(), fill))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFillMaxFilledAt pins the cursor-seed query: returns the
// newest filled_at for a project (or epoch for an empty project).
func TestFillMaxFilledAt(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()
	want := time.Date(2026, 6, 25, 18, 54, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(filled_at)")).
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(want))
	got, err := repo.MaxFilledAt(context.Background(), "p-1")
	require.NoError(t, err)
	assert.Equal(t, want, got.UTC())
}

// TestFillPatchCommission_OnlyWhenNull pins idempotency: the WHERE
// clause must guard on commission_usd IS NULL so a sweep-populated
// zero can't be overwritten by a late commission report.
// TestFillPatchCommission_OnlyWhenNull pins that the UPDATE carries the
// safety-critical "AND commission_usd IS NULL" guard. The regex matches
// both the SET and the WHERE clause so that dropping either fragment
// causes sqlmock to report "call to ExecContext, was not expected".
func TestFillPatchCommission_OnlyWhenNull(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()
	// Match must span both SET commission_usd and the IS NULL guard so the
	// test fails if either clause is removed from the implementation.
	mock.ExpectExec(
		`(?s)SET commission_usd.*WHERE id = \$2 AND commission_usd IS NULL`,
	).
		WithArgs(0.42, "exec-ex-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.PatchCommission(context.Background(), "exec-ex-1", 0.42))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFillRecordShadow_HappyPath pins that RecordShadow writes the
// 12-column INSERT into trading_fills_shadow with the same
// ON CONFLICT (id) DO NOTHING idempotency guarantee.
func TestFillRecordShadow_HappyPath(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()
	exec, acct, detail := "ex-2", "ACC2", "shadow"
	fill := &persistence.TradingFill{
		ID: "shadow-1", OrderID: "ord-2", ProjectID: "p-2", Symbol: "MSFT",
		Qty: 2, Price: 420.0, ExecID: &exec, AccountID: &acct,
		Source: "shadow", SourceDetail: &detail,
		FilledAt: time.Date(2026, 6, 25, 17, 0, 0, 0, time.UTC),
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_fills_shadow")).
		WithArgs("shadow-1", "ord-2", "p-2", "MSFT", 2.0, 420.0, nil,
			"ex-2", "ACC2", "shadow", "shadow",
			fill.FilledAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.RecordShadow(context.Background(), fill))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestFillRecordShadow_NilFillReturnsError guards the nil/empty-id
// validation gate on RecordShadow.
func TestFillRecordShadow_NilFillReturnsError(t *testing.T) {
	repo, _, cleanup := newFillRepo(t)
	defer cleanup()
	require.Error(t, repo.RecordShadow(context.Background(), nil))
	require.Error(t, repo.RecordShadow(context.Background(), &persistence.TradingFill{}))
}

// ---------- ListNullCommission ----------

// TestListNullCommission_ReturnsNullCommissionFills pins that
// ListNullCommission emits a SELECT filtering on commission_usd IS NULL
// and filled_at < $1, and that it maps exec_id + account_id correctly.
func TestListNullCommission_ReturnsNullCommissionFills(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	olderThan := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
	filledAt := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{
		"id", "exec_id", "account_id", "project_id", "symbol", "filled_at",
	}).AddRow("exec-abc", "abc", "DUH1", "proj-1", "AAPL", filledAt)

	mock.ExpectQuery(`commission_usd IS NULL AND filled_at < \$1`).
		WithArgs(olderThan).
		WillReturnRows(rows)

	got, err := repo.ListNullCommission(context.Background(), olderThan)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, got, 1)
	assert.Equal(t, "exec-abc", got[0].ID)
	require.NotNil(t, got[0].ExecID)
	assert.Equal(t, "abc", *got[0].ExecID)
	require.NotNil(t, got[0].AccountID)
	assert.Equal(t, "DUH1", *got[0].AccountID)
	assert.Equal(t, "proj-1", got[0].ProjectID)
	assert.Equal(t, "AAPL", got[0].Symbol)
	assert.Equal(t, filledAt.UTC(), got[0].FilledAt)
}

// TestListNullCommission_EmptyExecIDBecomesNil pins the COALESCE(”)
// → nil pointer contract: a fill with no exec_id should have a nil
// ExecID pointer, not a pointer to an empty string.
func TestListNullCommission_EmptyExecIDBecomesNil(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	olderThan := time.Date(2026, 6, 26, 15, 0, 0, 0, time.UTC)
	filledAt := time.Date(2026, 6, 26, 10, 0, 0, 0, time.UTC)

	// COALESCE(exec_id, '') returns "" when exec_id IS NULL.
	rows := sqlmock.NewRows([]string{
		"id", "exec_id", "account_id", "project_id", "symbol", "filled_at",
	}).AddRow("fill-no-exec", "", "", "proj-2", "MSFT", filledAt)

	mock.ExpectQuery(`commission_usd IS NULL`).
		WithArgs(olderThan).
		WillReturnRows(rows)

	got, err := repo.ListNullCommission(context.Background(), olderThan)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].ExecID, "empty exec_id must map to nil pointer")
	assert.Nil(t, got[0].AccountID, "empty account_id must map to nil pointer")
}

// TestListNullCommission_QueryError verifies that DB errors propagate.
func TestListNullCommission_QueryError(t *testing.T) {
	repo, mock, cleanup := newFillRepo(t)
	defer cleanup()

	mock.ExpectQuery(`commission_usd IS NULL`).
		WillReturnError(errors.New("conn closed"))

	_, err := repo.ListNullCommission(context.Background(), time.Now())
	require.Error(t, err)
}
