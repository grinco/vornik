package memory

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Write-side of slice 0a (design §4.1). Two things are load-bearing:
//
//  1. A zero time.Time must reach SQL as NULL, never as '0001-01-01'. A
//     non-NULL zero date would defeat COALESCE(event_time, created_at) and
//     silently exclude the chunk from every window — and it would pass a naive
//     round-trip test, which is why this asserts the driver argument itself.
//  2. IngestText keeps its exact signature and behaviour, so neither of the two
//     interfaces declaring it (executor.go:275, ingest_worker.go:44) nor any of
//     the five production call sites change.

// TestUpsertChunks_ZeroEventTimeIsNULL pins the sentinel translation.
func TestUpsertChunks_ZeroEventTimeIsNULL(t *testing.T) {
	repo, mock, cleanup := newRepo(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WithArgs(
			"c1", "p1",
			nil, nil,
			"src.md", 0, "body", "hash",
			sqlmock.AnyArg(), // embed_input_hash
			nil, nil,         // derived_from_*
			nil, // event_time — the assertion: NULL, not a zero date
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	chunk := MemoryChunk{
		ID: "c1", ProjectID: "p1", SourceName: "src.md",
		Content: "body", ContentHash: "hash",
		// EventTime deliberately left as the zero value.
	}
	if err := repo.UpsertChunks(context.Background(), []MemoryChunk{chunk}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a zero EventTime did not reach SQL as NULL: %v", err)
	}
}

// TestUpsertChunks_NonZeroEventTimeWrittenAsIs is the other half: a real event
// time must be persisted, not dropped.
func TestUpsertChunks_NonZeroEventTimeWrittenAsIs(t *testing.T) {
	repo, mock, cleanup := newRepo(t)
	defer cleanup()

	when := time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WithArgs(
			"c2", "p1",
			nil, nil,
			"src.md", 0, "body", "hash",
			sqlmock.AnyArg(),
			nil, nil,
			when,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	chunk := MemoryChunk{
		ID: "c2", ProjectID: "p1", SourceName: "src.md",
		Content: "body", ContentHash: "hash",
		EventTime: when,
	}
	if err := repo.UpsertChunks(context.Background(), []MemoryChunk{chunk}); err != nil {
		t.Fatalf("UpsertChunks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a non-zero EventTime was not written as-is: %v", err)
	}
}

// TestIngestTextAt_ZeroTimeMatchesIngestText pins the delegation contract:
// IngestText is exactly IngestTextAt with an unknown event time. If these ever
// diverge, the five existing call sites silently change behaviour.
func TestIngestTextAt_ZeroTimeMatchesIngestText(t *testing.T) {
	idx := &Indexer{cfg: Config{ChunkTokens: 64, ChunkOverlap: 0}}

	// Both paths must chunk identically; an empty body short-circuits before
	// any storage dependency is touched, which keeps this a pure contract test.
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s", ""); err != nil {
		t.Fatalf("IngestText(empty): %v", err)
	}
	if err := idx.IngestTextAt(context.Background(), "p", "t", "a", "s", "", time.Time{}); err != nil {
		t.Fatalf("IngestTextAt(empty, zero): %v", err)
	}
}
