package persistence

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
)

// TestBeginTx_RecognisesRawPool — a raw *sql.DB is a transaction
// beginner: ok=true and the returned Tx commits.
func TestBeginTx_RecognisesRawPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectCommit()

	tx, ok, err := BeginTx(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if !ok || tx == nil {
		t.Fatalf("raw *sql.DB should begin a tx: ok=%v tx=%v", ok, tx)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestBeginTx_RecognisesMetricsWrapper is the root-cause regression
// for the 2026-06-04 sweep: the daemon injects *DBWithMetrics, whose
// BeginTx returns *TxWithMetrics (not *sql.Tx). The old per-repo
// assertions missed it, panicking corpus rollback and silently
// dropping transactional inserts. BeginTx must recognise it.
func TestBeginTx_RecognisesMetricsWrapper(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	wrapped := NewDBWithMetrics(db, NewDBMetrics(prometheus.NewRegistry()), "test")

	mock.ExpectBegin()
	mock.ExpectCommit()

	tx, ok, err := BeginTx(context.Background(), wrapped, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if !ok || tx == nil {
		t.Fatalf("*DBWithMetrics should begin a tx: ok=%v tx=%v", ok, tx)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestBeginTx_PropagatesBeginError — a begin failure on the pool is
// surfaced with ok=true (a real beginner was found) and a nil Tx.
func TestBeginTx_PropagatesBeginError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	wrapped := NewDBWithMetrics(db, NewDBMetrics(prometheus.NewRegistry()), "test")

	mock.ExpectBegin().WillReturnError(errBeginBoom)

	tx, ok, err := BeginTx(context.Background(), wrapped, nil)
	if err == nil {
		t.Fatal("expected begin error to propagate")
	}
	if !ok {
		t.Errorf("ok should be true: a beginner was found, the begin just failed")
	}
	if tx != nil {
		t.Errorf("tx should be nil on begin error, got %v", tx)
	}
}

var errBeginBoom = errBoom("begin boom")

type errBoom string

func (e errBoom) Error() string { return string(e) }

// TestBeginTx_AlreadyInTransaction — when handed an *sql.Tx (an outer
// caller already owns the transaction), ok=false so the caller runs
// its statements directly without nesting.
func TestBeginTx_AlreadyInTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	outer, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx outer: %v", err)
	}

	tx, ok, err := BeginTx(context.Background(), outer, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if ok || tx != nil {
		t.Fatalf("*sql.Tx should not begin a nested tx: ok=%v tx=%v", ok, tx)
	}
	mock.ExpectRollback()
	_ = outer.Rollback()
}
