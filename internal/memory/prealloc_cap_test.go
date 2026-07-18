package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestListChunkContents_PreallocClamped is a regression test for the CodeQL
// go/uncontrolled-allocation-size finding on the result-slice pre-allocation.
// ListChunkContents clamps only the lower bound of `limit` (<=0 → 1000) and
// passes any positive caller value straight into `make([]string, 0, limit)`,
// so a pathological limit drove an unbounded pre-allocation. The fix clamps
// the capacity hint with min(limit, maxChunkContentsPrealloc).
//
// Observed via cap(out): a huge limit with few rows leaves cap at the clamped
// initial capacity (append never grows). Pre-fix cap would equal `limit`.
func TestListChunkContents_PreallocClamped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	rows := sqlmock.NewRows([]string{"content"})
	for i := 0; i < 3; i++ {
		rows.AddRow("chunk")
	}
	const huge = 1 << 30
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p1", huge).
		WillReturnRows(rows)

	got, err := repo.ListChunkContents(context.Background(), "p1", huge)
	if err != nil {
		t.Fatalf("ListChunkContents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListChunkContents returned %d rows, want 3", len(got))
	}
	if cap(got) > maxChunkContentsPrealloc {
		t.Errorf("ListChunkContents pre-allocated cap=%d for limit=%d; want clamp to <= %d (uncontrolled-allocation-size regression)", cap(got), huge, maxChunkContentsPrealloc)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestRecentChunkPreallocCap pins the belt-and-suspenders cap on
// ListRecentChunks' pre-allocation. `limit` there is already clamped to
// [5,50] upstream, but the make-site guard exists so CodeQL sees a bounded
// allocation regardless. The cap must cover the real upper clamp (50) so a
// legitimate request is never under-allocated in a way that matters, and must
// still clamp a pathological value.
func TestRecentChunkPreallocCap(t *testing.T) {
	if maxRecentChunkPrealloc < 50 {
		t.Errorf("maxRecentChunkPrealloc=%d must cover the documented ListRecentChunks upper clamp of 50", maxRecentChunkPrealloc)
	}
	const huge = 1 << 30
	if got := min(huge, maxRecentChunkPrealloc); got != maxRecentChunkPrealloc {
		t.Errorf("min(%d, %d) = %d, want clamp to %d", huge, maxRecentChunkPrealloc, got, maxRecentChunkPrealloc)
	}
	if got := min(7, maxRecentChunkPrealloc); got != 7 {
		t.Errorf("legitimate size 7 must pass through, got %d", got)
	}
}
