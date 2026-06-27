package persistence

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestDBWithMetrics_QueryContext_RecordsSuccessAndError exercises the wrapper's
// QueryContext path on both happy and error branches via sqlmock.
func TestDBWithMetrics_QueryContext_RecordsSuccessAndError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"col"}).AddRow(1))
	rows, err := wrapped.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	_ = rows.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 2")).
		WillReturnError(errors.New("boom"))
	if _, err := wrapped.QueryContext(context.Background(), "SELECT 2"); err == nil {
		t.Fatalf("expected error, got nil")
	}

	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "query", "success")); v != 1 {
		t.Errorf("expected 1 success, got %v", v)
	}
	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "query", "error")); v != 1 {
		t.Errorf("expected 1 error, got %v", v)
	}
}

// TestDBWithMetrics_QueryRowContext_RecordsCall ensures QueryRowContext
// records a query_row data point (no error path because *sql.Row defers
// reporting until Scan).
func TestDBWithMetrics_QueryRowContext_RecordsCall(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 42")).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(42))
	var got int
	if err := wrapped.QueryRowContext(context.Background(), "SELECT 42").Scan(&got); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "query_row", "success")); v != 1 {
		t.Errorf("expected 1 query_row success, got %v", v)
	}
}

// TestDBWithMetrics_ExecContext_SuccessAndError verifies ExecContext labels
// success vs error.
func TestDBWithMetrics_ExecContext_SuccessAndError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectExec(regexp.QuoteMeta("UPDATE t SET v=1")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := wrapped.ExecContext(context.Background(), "UPDATE t SET v=1"); err != nil {
		t.Fatalf("ExecContext success: %v", err)
	}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE t SET v=2")).
		WillReturnError(errors.New("boom"))
	if _, err := wrapped.ExecContext(context.Background(), "UPDATE t SET v=2"); err == nil {
		t.Fatalf("expected error")
	}

	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "exec", "success")); v != 1 {
		t.Errorf("expected 1 exec success, got %v", v)
	}
	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "exec", "error")); v != 1 {
		t.Errorf("expected 1 exec error, got %v", v)
	}
}

// TestDBWithMetrics_BeginTx_RecordsBegin covers the begin path; the returned
// transaction itself is the target of the tx-tests below.
func TestDBWithMetrics_BeginTx_RecordsBegin(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectBegin()
	mock.ExpectRollback()

	tx, err := wrapped.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "begin", "success")); v != 1 {
		t.Errorf("expected 1 begin success, got %v", v)
	}
	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "rollback", "success")); v != 1 {
		t.Errorf("expected 1 rollback success, got %v", v)
	}
}

// TestDBWithMetrics_BeginTx_Error covers the failure branch.
func TestDBWithMetrics_BeginTx_Error(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectBegin().WillReturnError(errors.New("nope"))
	if _, err := wrapped.BeginTx(context.Background(), nil); err == nil {
		t.Fatalf("expected error")
	}
	if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", "begin", "error")); v != 1 {
		t.Errorf("expected 1 begin error, got %v", v)
	}
}

// TestTxWithMetrics_AllMethods walks every Tx-wrapper method to ensure each
// records the right operation label.
func TestTxWithMetrics_AllMethods(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE t SET v=1")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1")).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 2")).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
	mock.ExpectCommit()

	tx, err := wrapped.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), "UPDATE t SET v=1"); err != nil {
		t.Fatalf("Tx.Exec: %v", err)
	}
	rows, err := tx.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Tx.Query: %v", err)
	}
	_ = rows.Close()
	var v int
	if err := tx.QueryRowContext(context.Background(), "SELECT 2").Scan(&v); err != nil {
		t.Fatalf("Tx.QueryRow: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Tx.Commit: %v", err)
	}

	cases := map[string]float64{
		"exec":      1,
		"query":     1,
		"query_row": 1,
		"commit":    1,
	}
	for op, want := range cases {
		if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", op, "success")); v != want {
			t.Errorf("op=%s want=%v got=%v", op, want, v)
		}
	}
}

// TestTxWithMetrics_ErrorPaths covers Exec/Query/Commit/Rollback error branches.
func TestTxWithMetrics_ErrorPaths(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "testdb")

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("BAD")).WillReturnError(errors.New("exec boom"))
	mock.ExpectQuery(regexp.QuoteMeta("BAD QUERY")).WillReturnError(errors.New("query boom"))
	mock.ExpectCommit().WillReturnError(errors.New("commit boom"))

	tx, err := wrapped.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), "BAD"); err == nil {
		t.Fatal("expected exec error")
	}
	if _, err := tx.QueryContext(context.Background(), "BAD QUERY"); err == nil {
		t.Fatal("expected query error")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("expected commit error")
	}

	// Independent tx for rollback error
	mock.ExpectBegin()
	mock.ExpectRollback().WillReturnError(errors.New("rollback boom"))
	tx2, err := wrapped.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx 2: %v", err)
	}
	if err := tx2.Rollback(); err == nil {
		t.Fatal("expected rollback error")
	}

	for _, op := range []string{"exec", "query", "commit", "rollback"} {
		if v := testutil.ToFloat64(metrics.QueryTotal.WithLabelValues("testdb", op, "error")); v != 1 {
			t.Errorf("op=%s expected 1 error, got %v", op, v)
		}
	}
}

// TestDBWithMetrics_RecordPoolStats_NilMetricsIsSafe ensures the no-op path
// runs without panic when metrics or DB are nil.
func TestDBWithMetrics_RecordPoolStats_NilMetricsIsSafe(t *testing.T) {
	w := &DBWithMetrics{DB: nil, metrics: nil, database: "x"}
	w.RecordPoolStats()

	w2 := &DBWithMetrics{DB: &sql.DB{}, metrics: nil, database: "x"}
	w2.RecordPoolStats()
}

// TestDBWithMetrics_RecordPoolStats_HappyPath drives the real Stats() call
// against a sqlmock-backed *sql.DB so that the gauge updates are exercised.
func TestDBWithMetrics_RecordPoolStats_HappyPath(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := prometheus.NewRegistry()
	metrics := NewDBMetrics(reg)
	wrapped := NewDBWithMetrics(db, metrics, "poolstats")
	wrapped.RecordPoolStats()
	// Gauge should now exist; assert via testutil
	_ = testutil.ToFloat64(metrics.OpenConnections.WithLabelValues("poolstats"))
}

// TestDBMetrics_InitializeQuerySeries_NilSafe exercises the zero-database
// short-circuit explicitly.
func TestDBMetrics_InitializeQuerySeries_NilSafe(t *testing.T) {
	var m *DBMetrics
	m.initializeQuerySeries("x")
	m = NewDBMetrics(prometheus.NewRegistry())
	m.initializeQuerySeries("")
}
