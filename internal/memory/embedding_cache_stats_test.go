package memory

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCacheStats_TablePresent — happy path covers the three SQL
// queries (to_regclass + COUNT + pg_total_relation_size) and pins
// that all three values land on the returned struct.
func TestCacheStats_TablePresent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := &embeddingCacheRepo{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("to_regclass('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COUNT(DISTINCT model) FROM embedding_cache")).
		WillReturnRows(sqlmock.NewRows([]string{"count", "distinct_models"}).AddRow(int64(1234), 2))
	mock.ExpectQuery(regexp.QuoteMeta("pg_total_relation_size('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(int64(5_242_880))) // 5 MiB

	got, err := r.CacheStats(context.Background())
	if err != nil {
		t.Fatalf("CacheStats: %v", err)
	}
	if got.RowCount != 1234 {
		t.Errorf("RowCount = %d, want 1234", got.RowCount)
	}
	if got.DistinctModels != 2 {
		t.Errorf("DistinctModels = %d, want 2", got.DistinctModels)
	}
	if got.ApproxBytes != 5_242_880 {
		t.Errorf("ApproxBytes = %d, want 5242880", got.ApproxBytes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestCacheStats_TableAbsent — older deployment where migration 41
// hasn't been applied yet. to_regclass returns NULL → the helper
// short-circuits with zero values + nil error so the spend panel
// renders "disabled" rather than 500.
func TestCacheStats_TableAbsent(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	r := &embeddingCacheRepo{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("to_regclass('public.embedding_cache')")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))

	got, err := r.CacheStats(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got.RowCount != 0 || got.DistinctModels != 0 || got.ApproxBytes != 0 {
		t.Errorf("expected zero stats for absent table; got %+v", got)
	}
}

// TestCacheStats_NilReceiver — nil safety. The UI option wires nil
// when the cache isn't enabled; CacheStats must return cleanly
// rather than panicking.
func TestCacheStats_NilReceiver(t *testing.T) {
	var r *embeddingCacheRepo
	got, err := r.CacheStats(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got.RowCount != 0 {
		t.Errorf("nil receiver should yield zero stats, got %+v", got)
	}
}

// TestCacheStats_PresenceQueryError — a transient DB error on the
// to_regclass probe must propagate so the operator sees the failure
// rather than silently rendering "disabled" (which would mask a
// real outage).
func TestCacheStats_PresenceQueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	r := &embeddingCacheRepo{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("to_regclass('public.embedding_cache')")).
		WillReturnError(errors.New("simulated connection reset"))

	_, err := r.CacheStats(context.Background())
	if err == nil {
		t.Error("expected error from presence probe")
	}
}
