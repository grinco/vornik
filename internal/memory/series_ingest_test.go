package memory

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Writing `series_key` at ingest (2026-09-16-retrieval-recency-design.md
// §5.1.1). The derivation RULE is tested in series_test.go and the container
// SEAM in internal/service; what is pinned here is the one step between them —
// that the resolved key reaches the INSERT, once per artifact, and that an
// unwired resolver is a clean no-op rather than a half-enabled state.
//
// The distinction matters because §5.3's supersession reads the column and
// nothing else: a key that is derived correctly and then dropped on the way to
// the database is indistinguishable, from every query, from no feature at all.

// chunkInsertArgs is the number of bind parameters UpsertChunks sends per
// chunk, with series_key last. Pinned as a constant so a column added ahead of
// series_key fails these tests loudly instead of silently shifting which
// parameter they assert on.
const chunkInsertArgs = 13

// expectChunkInsert asserts one chunk INSERT whose series_key parameter is
// wantSeries (nil for SQL NULL), leaving every other parameter unconstrained —
// those are covered by the existing indexer tests and restating them here would
// make this test fail for reasons that are not its subject.
func expectChunkInsert(mock sqlmock.Sqlmock, wantSeries driver.Value) {
	args := make([]driver.Value, 0, chunkInsertArgs)
	for i := 0; i < chunkInsertArgs-1; i++ {
		args = append(args, sqlmock.AnyArg())
	}
	args = append(args, wantSeries)
	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// countingResolver records how often it was asked. The count is the point of
// one of the tests below: series membership is a property of the ARTIFACT, so
// asking per chunk would issue N task lookups per ingest for an answer that
// cannot differ between them.
type countingResolver struct {
	key   string
	warn  bool
	calls int

	gotProject string
	gotTask    string
}

func (c *countingResolver) SeriesKeyFor(_ context.Context, projectID, taskID string) (string, bool) {
	c.calls++
	c.gotProject, c.gotTask = projectID, taskID
	return c.key, c.warn
}

func TestIngestText_ResolvedSeriesKeyReachesTheInsert(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	res := &countingResolver{key: "czech-news"}
	idx.SetSeriesResolver(res)

	expectChunkInsert(mock, "czech-news")
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "assistant", "task_1", "art_1", "s.md", "the daily digest"); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("series_key did not reach the insert: %v", err)
	}
	if res.gotProject != "assistant" || res.gotTask != "task_1" {
		t.Errorf("resolver asked about (%q, %q), want (assistant, task_1)", res.gotProject, res.gotTask)
	}
}

// No resolver wired = feature off. Not "wired and always empty": the chunks
// must still be written, with series_key NULL, and the ingest must not error.
// This is the state every deployment is in until the container wires one, and
// every test fixture that constructs an Indexer directly.
func TestIngestText_NilResolverWritesNullAndDoesNotError(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	expectChunkInsert(mock, nil)
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "assistant", "task_1", "art_1", "s.md", "an ordinary artifact"); err != nil {
		t.Fatalf("a nil resolver must be a no-op, got error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("nil resolver: %v", err)
	}
}

// The overwhelming majority of chunks: a resolver is live, and this task is
// simply not part of a recurring series. Empty must persist as NULL, not as the
// empty string — §5.3's EXISTS is guarded by `series_key IS NOT NULL` and the
// partial index excludes NULLs, so an empty-string key would put 99% of the
// store into the index and give every non-series chunk a "series" of its own.
func TestIngestText_ResolverWithNoSeriesWritesNull(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	res := &countingResolver{key: ""}
	idx.SetSeriesResolver(res)

	expectChunkInsert(mock, nil)
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "janka", "task_2", "art_2", "s.md", "an ad-hoc task output"); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no-series resolver: %v", err)
	}
	if res.calls != 1 {
		t.Errorf("resolver calls = %d, want 1", res.calls)
	}
}

// One artifact's chunks all belong to the same series, so the question is asked
// once per ingest and the answer stamped on every chunk. Asking per chunk would
// turn a 40-chunk digest into 40 task-store round trips for an answer that
// cannot vary.
func TestIngestText_SeriesKeyResolvedOncePerIngestNotPerChunk(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	// 5 tokens ≈ 20 bytes: the two 15-byte paragraphs below cannot share a
	// chunk (15+15+2 > 20) and neither is large enough to be split further, so
	// this ingest is exactly two chunks.
	idx.cfg.ChunkTokens = 5
	idx.cfg.ChunkOverlap = 0

	res := &countingResolver{key: "prague-events"}
	idx.SetSeriesResolver(res)

	expectChunkInsert(mock, "prague-events")
	expectChunkInsert(mock, "prague-events")
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "assistant", "task_3", "art_3", "s.md",
		"digest part one\n\ndigest part two"); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("multi-chunk ingest: %v", err)
	}
	if res.calls != 1 {
		t.Errorf("resolver calls = %d for a 2-chunk ingest, want 1 — membership is a "+
			"property of the artifact, not of each chunk", res.calls)
	}
}

// The repository leg on its own, because the two drivers share exactly one
// INSERT statement and the only thing that keeps them in step is that both
// schemas carry the column. A Go-side empty string must become SQL NULL here
// and not at some caller.
func TestUpsertChunks_SeriesKeyNullWhenEmpty(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()

	expectChunkInsert(mock, nil)
	expectChunkInsert(mock, "czech-news")

	err := r.UpsertChunks(context.Background(), []MemoryChunk{
		{ID: "c1", ProjectID: "p", SourceName: "s.md", Content: "x", ContentHash: "h1"},
		{ID: "c2", ProjectID: "p", SourceName: "s.md", Content: "y", ContentHash: "h2", SeriesKey: "czech-news"},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("series_key column: %v", err)
	}
}

// A resolver that warns still yields its (empty) key without failing the
// ingest: §5.1.1's undeclared-slug row is a signal, not a gate. The chunks are
// written, unkeyed, which is precisely the pre-change behaviour.
func TestIngestText_WarningResolverStillIngests(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	idx.SetSeriesResolver(&countingResolver{key: "", warn: true})

	expectChunkInsert(mock, nil)
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "assistant", "task_4", "art_4", "s.md", "digest body"); err != nil {
		t.Fatalf("a warning must not fail the ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("warning path: %v", err)
	}
}

// SetSeriesResolver on a nil Indexer must not panic — it mirrors
// SetAutoStampPolicies, which the container may call on a memory manager that
// failed to initialise.
func TestSetSeriesResolver_NilIndexerIsSafe(*testing.T) {
	var idx *Indexer
	idx.SetSeriesResolver(&countingResolver{})
}
