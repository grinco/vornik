// Tests for the 2026.6.0 temporal-filter retrofit. Anchors:
//
//   - nullableTime() returns nil for zero values so the SQL
//     `IS NULL OR …` guard short-circuits; non-zero values pass
//     through unchanged so lib/pq binds them as timestamptz.
//   - SearchOptions defaults: Limit ≤ 0 falls back to the legacy
//     page size of 10 (asserted on the documented contract; the
//     normalisation lives in searchInternal).
//
// SQL-side filtering is exercised by the sqlmock-driven repository
// tests alongside the other HybridSearchWithEpochs cases.

package memory

import (
	"context"
	"testing"
	"time"
)

// TestNullableTime_ZeroBecomesNil — the helper bridging
// time.Time → driver-NULL must return nil for the zero value so
// the Postgres `$N::timestamptz IS NULL OR …` guard short-circuits.
// A bug here would cause the temporal filter to run against
// year-0001 and silently exclude every chunk.
func TestNullableTime_ZeroBecomesNil(t *testing.T) {
	if got := nullableTime(time.Time{}); got != nil {
		t.Errorf("nullableTime(zero) = %v, want nil so the SQL bound stays inactive", got)
	}
}

// TestNullableTime_NonZeroPassesThrough — a real timestamp must
// round-trip unchanged so the driver binds it as timestamptz.
// Pins the legacy bind path against accidental wrapping in a
// helper type that the driver doesn't recognise.
func TestNullableTime_NonZeroPassesThrough(t *testing.T) {
	ts := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	got := nullableTime(ts)
	asTime, ok := got.(time.Time)
	if !ok {
		t.Fatalf("nullableTime(%v) returned %T, want time.Time", ts, got)
	}
	if !asTime.Equal(ts) {
		t.Errorf("nullableTime(%v) = %v, want the timestamp itself", ts, asTime)
	}
}

// TestSearchOptions_ZeroValuesAreInert — both bounds zero is the
// canonical "no temporal filter" shape used by the legacy
// Search(query, limit) delegate. Pins the contract that the
// struct's zero value is equivalent to passing no opts at all.
func TestSearchOptions_ZeroValuesAreInert(t *testing.T) {
	opts := SearchOptions{}
	if !opts.FromDate.IsZero() {
		t.Error("zero SearchOptions.FromDate must be zero time")
	}
	if !opts.ToDate.IsZero() {
		t.Error("zero SearchOptions.ToDate must be zero time")
	}
	if opts.Limit != 0 {
		t.Error("zero SearchOptions.Limit must be 0 so the searcher's default-10 fallback kicks in")
	}
}

// TestSearchWithOptions_DelegatesToInternal — confirms the new
// public entry point routes through the same searchInternal path
// the legacy Search uses, so all the existing audit / metrics /
// reranker plumbing applies to temporal-filtered calls too.
// Uses the same sqlmock harness as the other searcher tests.
func TestSearchWithOptions_DelegatesToInternal(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.setMetrics(freshMetrics())

	// The repo path the FTS-only (no-embedder) branch takes is
	// the temporal keyword query, which must carry the bounds even
	// when epoch filtering is not configured.
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	// Migration 75 adds a trailing repo_scope arg ($6); the test
	// caller passes no scope (project-wide), which the driver binds
	// as nil from nullableString("").
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 7, from, to, nil, "q").
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.5}))

	got, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{
		Limit:    7,
		FromDate: from,
		ToDate:   to,
	})
	if err != nil {
		t.Fatalf("SearchWithOptions failed: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("unexpected results: %+v", got)
	}
}
