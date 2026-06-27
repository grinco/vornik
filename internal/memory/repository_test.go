package memory

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEscapeLikeWildcards(t *testing.T) {
	cases := map[string]string{
		"plain":   "plain",
		"50%":     `50\%`,
		"a_b":     `a\_b`,
		`back\sl`: `back\\sl`,
		"%_\\":    `\%\_\\`,
	}
	for in, want := range cases {
		if got := escapeLikeWildcards(in); got != want {
			t.Errorf("escapeLikeWildcards(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSubstringSearch_EscapesWildcards is the hardening regression
// (2026-06-15): the tier-3 substring fallback must bind the ESCAPED
// query (so % / _ are literals, paired with ESCAPE '\'), not the raw
// text that would act as wildcards.
func TestSubstringSearch_EscapesWildcards(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	cols := []string{"id", "project_id", "task_id", "source_name", "content",
		"score", "content_class", "is_alive", "last_checked_at"}
	mock.ExpectQuery("content ILIKE.*ESCAPE").
		WithArgs("p", `50\%\_x`, 10, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(cols))
	if _, err := r.substringSearch(context.Background(), "p", "50%_x", 10, "", false); err != nil {
		t.Fatalf("substringSearch: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func newRepo(t *testing.T) (*Repository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewRepository(db), mock, func() { _ = db.Close() }
}

func TestPgvectorAvailable_TrueAndCached(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if !r.pgvectorAvailable(context.Background()) {
		t.Fatalf("want true")
	}
	// Second call must not query again (sync.Once).
	if !r.pgvectorAvailable(context.Background()) {
		t.Fatalf("cache lost")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPgvectorAvailable_QueryErrorReturnsFalse(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").WillReturnError(errors.New("nope"))
	if r.pgvectorAvailable(context.Background()) {
		t.Fatalf("want false on err")
	}
}

func TestUpsertChunks_EmptyAndError(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if err := r.UpsertChunks(context.Background(), nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	chunks := []MemoryChunk{{ID: "c1", ProjectID: "p1", ContentHash: "h"}}
	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WithArgs("c1", "p1", "", "", "", 0, "", "h").
		WillReturnError(errors.New("boom"))
	if err := r.UpsertChunks(context.Background(), chunks); err == nil {
		t.Fatal("want error")
	}
}

func TestUpsertChunks_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	chunks := []MemoryChunk{
		{ID: "c1", ProjectID: "p", TaskID: "t", ArtifactID: "a", SourceName: "s", ChunkIndex: 0, Content: "x", ContentHash: "h1"},
		{ID: "c2", ProjectID: "p", TaskID: "t", ArtifactID: "a", SourceName: "s", ChunkIndex: 1, Content: "y", ContentHash: "h2"},
	}
	for _, c := range chunks {
		// derived_from_* are NULL for chunks without document-
		// extraction provenance (the legacy markdown-OUTPUT path).
		// sqlmock matches a nil *string against the (*string)(nil)
		// the driver receives, so the assertion remains exact.
		var nilStr *string
		mock.ExpectExec("INSERT INTO project_memory_chunks").
			WithArgs(c.ID, c.ProjectID, c.TaskID, c.ArtifactID, c.SourceName, c.ChunkIndex, c.Content, c.ContentHash, nilStr, nilStr).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	if err := r.UpsertChunks(context.Background(), chunks); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestUpsertChunks_WithDocumentProvenance pins the SQL shape for
// chunks produced by the document-ingest path. The new
// derived_from_extracted_document_id + derived_from_section_id
// columns must land as non-NULL when the caller populates them.
func TestUpsertChunks_WithDocumentProvenance(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	docID := "extdoc_1"
	secID := "001-chapter-one"
	chunk := MemoryChunk{
		ID:                             "c1",
		ProjectID:                      "p",
		TaskID:                         "t",
		ArtifactID:                     "a",
		SourceName:                     "Book · Chapter One",
		ChunkIndex:                     0,
		Content:                        "x",
		ContentHash:                    "h1",
		DerivedFromExtractedDocumentID: docID,
		DerivedFromSectionID:           secID,
	}
	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WithArgs(chunk.ID, chunk.ProjectID, chunk.TaskID, chunk.ArtifactID, chunk.SourceName,
			chunk.ChunkIndex, chunk.Content, chunk.ContentHash, &docID, &secID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpsertChunks(context.Background(), []MemoryChunk{chunk}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueForEmbedding(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	if err := r.EnqueueForEmbedding(context.Background(), nil); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestEnqueueForEmbedding_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WithArgs("a", "b").
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := r.EnqueueForEmbedding(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
}

func TestListUnclassifiedChunks(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.ListUnclassifiedChunks(context.Background(), "p", 10); got != nil || err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Empty project ID → no-op.
	if got, _ := r.ListUnclassifiedChunks(context.Background(), "", 10); got != nil {
		t.Fatal("empty project")
	}
	// Limit clamping: 0 → 100.
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "doc.md", "researcher", "body"))
	got, err := r.ListUnclassifiedChunks(context.Background(), "p", 0)
	if err != nil || len(got) != 1 || got[0].ID != "c1" || got[0].Content != "body" {
		t.Fatalf("got %+v %v", got, err)
	}
	// Limit clamping: huge → 1000.
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 1000).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	if _, err := r.ListUnclassifiedChunks(context.Background(), "p", 9999); err != nil {
		t.Fatal(err)
	}
	// Error path.
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnError(errors.New("boom"))
	if _, err := r.ListUnclassifiedChunks(context.Background(), "p", 10); err == nil {
		t.Fatal("want err")
	}
}

// Cross-project sibling — same shape as TestListUnclassifiedChunks
// but exercises the all-projects path used by the auto-backfill loop.
func TestListUnclassifiedChunksAcrossProjects(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.ListUnclassifiedChunksAcrossProjects(context.Background(), 10); got != nil || err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Limit clamping: 0 → 100, takes a single positional arg (no project_id).
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs(100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p-alpha", "alpha.md", "researcher", "body alpha").
			AddRow("c2", "p-beta", "beta.md", "", "body beta"))
	got, err := r.ListUnclassifiedChunksAcrossProjects(context.Background(), 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	if got[0].ProjectID != "p-alpha" || got[1].ProjectID != "p-beta" {
		t.Fatalf("project IDs preserved across rows: %+v", got)
	}

	// Limit clamping: huge → 1000.
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs(1000).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	if _, err := r.ListUnclassifiedChunksAcrossProjects(context.Background(), 9999); err != nil {
		t.Fatal(err)
	}

	// Error path.
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("db down"))
	if _, err := r.ListUnclassifiedChunksAcrossProjects(context.Background(), 10); err == nil {
		t.Fatal("want err")
	}
}

func TestCountUnclassifiedChunks(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.CountUnclassifiedChunks(context.Background()); got != 0 || err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	got, err := r.CountUnclassifiedChunks(context.Background())
	if err != nil || got != 42 {
		t.Fatalf("got %d %v", got, err)
	}
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("boom"))
	if _, err := r.CountUnclassifiedChunks(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

func TestUpdateChunkClass(t *testing.T) {
	var nilR *Repository
	if err := nilR.UpdateChunkClass(context.Background(), "c", "research", 0); err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// No-op guards.
	if err := r.UpdateChunkClass(context.Background(), "", "research", 0); err != nil {
		t.Fatal("empty chunk id")
	}
	if err := r.UpdateChunkClass(context.Background(), "c", "", 0); err != nil {
		t.Fatal("empty class")
	}

	// TTL=0 → NULL expires_at.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateChunkClass(context.Background(), "c1", "decision", 0); err != nil {
		t.Fatal(err)
	}

	// TTL>0 → expires_at = NOW() + interval.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateChunkClass(context.Background(), "c2", "research", 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	// Error path.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnError(errors.New("boom"))
	if err := r.UpdateChunkClass(context.Background(), "c3", "research", 0); err == nil {
		t.Fatal("want err")
	}
}

func TestCountUnclassifiedByRole(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.CountUnclassifiedByRole(context.Background(), "p"); got != nil || err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if got, _ := r.CountUnclassifiedByRole(context.Background(), ""); got != nil {
		t.Fatal("empty project")
	}
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).
			AddRow("researcher", 3).
			AddRow("coder", 7).
			AddRow("", 2))
	got, err := r.CountUnclassifiedByRole(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if got["researcher"] != 3 || got["coder"] != 7 || got[""] != 2 {
		t.Fatalf("got %+v", got)
	}
	// Error path.
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnError(errors.New("boom"))
	if _, err := r.CountUnclassifiedByRole(context.Background(), "p"); err == nil {
		t.Fatal("want err")
	}
}

func TestReclassifyUnclassifiedByRoles(t *testing.T) {
	var nilR *Repository
	if n, err := nilR.ReclassifyUnclassifiedByRoles(context.Background(), "p", "research", []string{"x"}, 0); n != 0 || err != nil {
		t.Fatal("nil-safe")
	}
	r, _, cleanup := newRepo(t)
	defer cleanup()
	// Empty arguments are no-ops.
	if n, _ := r.ReclassifyUnclassifiedByRoles(context.Background(), "", "c", []string{"x"}, 0); n != 0 {
		t.Fatal("empty project")
	}
	if n, _ := r.ReclassifyUnclassifiedByRoles(context.Background(), "p", "", []string{"x"}, 0); n != 0 {
		t.Fatal("empty class")
	}
	if n, _ := r.ReclassifyUnclassifiedByRoles(context.Background(), "p", "c", nil, 0); n != 0 {
		t.Fatal("empty roles")
	}

	r2, mock, cleanup2 := newRepo(t)
	defer cleanup2()
	// TTL=0 → NULL expires_at.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 5))
	n, err := r2.ReclassifyUnclassifiedByRoles(context.Background(), "p", "research", []string{"researcher", "scout"}, 0)
	if err != nil || n != 5 {
		t.Fatalf("got %d %v", n, err)
	}

	// TTL>0 → expires_at = NOW() + interval.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 2))
	n, err = r2.ReclassifyUnclassifiedByRoles(context.Background(), "p", "diagnostic", []string{"tester"}, 7*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("ttl: %d %v", n, err)
	}

	// Error path.
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnError(errors.New("boom"))
	if _, err := r2.ReclassifyUnclassifiedByRoles(context.Background(), "p", "c", []string{"x"}, 0); err == nil {
		t.Fatal("want err")
	}
}

func TestRequeueAllForEmbedding(t *testing.T) {
	var nilR *Repository
	if n, err := nilR.RequeueAllForEmbedding(context.Background(), "p"); n != 0 || err != nil {
		t.Fatal("nil-safe")
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Empty project ID → no-op.
	if n, _ := r.RequeueAllForEmbedding(context.Background(), ""); n != 0 {
		t.Fatal("empty project")
	}
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WithArgs("p1").
		WillReturnResult(sqlmock.NewResult(0, 42))
	n, err := r.RequeueAllForEmbedding(context.Background(), "p1")
	if err != nil || n != 42 {
		t.Fatalf("got %d %v", n, err)
	}
	// Error path.
	mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnError(errors.New("boom"))
	if _, err := r.RequeueAllForEmbedding(context.Background(), "p1"); err == nil {
		t.Fatal("want err")
	}
}

func TestDequeueEmbedBatch_EmptyQueue(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// 2026-05-29 audit fix: DequeueEmbedBatch wraps DELETE-RETURNING
	// + chunk SELECT in one tx so a crash between them can't
	// silently strand chunks. Empty-queue still commits.
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM memory_embed_queue").
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}))
	mock.ExpectCommit()
	got, err := r.DequeueEmbedBatch(context.Background(), 10)
	if err != nil || got != nil {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestDequeueEmbedBatch_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM memory_embed_queue").
		WithArgs(2).
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1").AddRow("c2"))

	createdAt := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT id, project_id").
		WithArgs("c1", "c2").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "artifact_id", "source_name",
			"chunk_index", "content", "content_hash", "created_at",
		}).
			AddRow("c1", "p", "t", "a", "s", 0, "alpha", "h1", createdAt).
			AddRow("c2", "p", "t", "a", "s", 1, "beta", "h2", createdAt))
	mock.ExpectCommit()

	got, err := r.DequeueEmbedBatch(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "alpha" {
		t.Fatalf("got %+v", got)
	}
}

func TestDequeueEmbedBatch_DequeueErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("DELETE FROM memory_embed_queue").WillReturnError(errors.New("x"))
	if _, err := r.DequeueEmbedBatch(context.Background(), 1); err == nil {
		t.Fatal("want err")
	}
}

func TestDequeueEmbedBatch_FetchErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("DELETE FROM memory_embed_queue").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnError(errors.New("x"))
	if _, err := r.DequeueEmbedBatch(context.Background(), 1); err == nil {
		t.Fatal("want err")
	}
}

// TestDequeueEmbedBatch_FetchErrRollsBackDelete — regression test
// for the 2026-05-29 audit-agent finding: pre-fix the DELETE-
// RETURNING and chunk SELECT were separate auto-committed
// statements, so a fetchChunksByIDs error left the queue rows
// purged without the worker getting anything back — silent
// permanent chunk loss. Now both run in one tx; the fetch error
// must roll back the DELETE. Pin the rollback expectation so a
// future refactor that drops the tx wrapper gets caught.
func TestDequeueEmbedBatch_FetchErrRollsBackDelete(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM memory_embed_queue").
		WillReturnRows(sqlmock.NewRows([]string{"chunk_id"}).AddRow("c1"))
	mock.ExpectQuery("SELECT id, project_id").
		WillReturnError(errors.New("fetch boom"))
	mock.ExpectRollback()
	if _, err := r.DequeueEmbedBatch(context.Background(), 1); err == nil {
		t.Fatal("want fetch err to bubble")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rollback contract: %v", err)
	}
}

func TestUpdateEmbedding(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// 2026-05-29 audit fix: empty embedding returns
	// ErrEmptyEmbedding so the worker can distinguish "stored OK"
	// from "model returned nothing for this chunk". Pre-fix this
	// was a silent nil return.
	if err := r.UpdateEmbedding(context.Background(), "c1", nil); !errors.Is(err, ErrEmptyEmbedding) {
		t.Fatalf("expected ErrEmptyEmbedding for nil slice, got %v", err)
	}
	mock.ExpectExec("UPDATE project_memory_chunks SET embedding").
		WithArgs("[1.5,2]", "c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateEmbedding(context.Background(), "c1", []float32{1.5, 2}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateContentTitle(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// nil repo / empty title → no-op.
	var nilR *Repository
	_ = nilR.UpdateContentTitle(context.Background(), "c", "t")
	if err := r.UpdateContentTitle(context.Background(), "", "t"); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateContentTitle(context.Background(), "c", "   "); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE project_memory_chunks SET content_title").
		WithArgs("clean", "c").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateContentTitle(context.Background(), "c", " clean "); err != nil {
		t.Fatal(err)
	}
}

func TestCountChunksMissingTitle(t *testing.T) {
	var nilR *Repository
	if n, err := nilR.CountChunksMissingTitle(context.Background()); n != 0 || err != nil {
		t.Fatalf("nil-safe: %d %v", n, err)
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	n, err := r.CountChunksMissingTitle(context.Background())
	if err != nil || n != 42 {
		t.Fatalf("got %d %v", n, err)
	}
	// Error path.
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("boom"))
	if _, err := r.CountChunksMissingTitle(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

func TestListChunksMissingTitle(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.ListChunksMissingTitle(context.Background(), 10); got != nil || err != nil {
		t.Fatalf("nil-safe: %v %v", got, err)
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Clamp at 1000 (over-cap) and reverse-clamp to 100 (zero).
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WithArgs(1000).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "x"))
	got, err := r.ListChunksMissingTitle(context.Background(), 5000)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WithArgs(100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}))
	got, err = r.ListChunksMissingTitle(context.Background(), 0)
	if err != nil || got != nil {
		t.Fatalf("got %v %v", got, err)
	}
	// Query error.
	mock.ExpectQuery("SELECT id, project_id, source_name, content").WillReturnError(errors.New("x"))
	if _, err := r.ListChunksMissingTitle(context.Background(), 10); err == nil {
		t.Fatal("want err")
	}
}

func TestExtractContentTitle(t *testing.T) {
	// Title field wins.
	if got := extractContentTitle("  Topic  ", "# heading", "fallback.md"); got != "Topic" {
		t.Fatalf("title wins: %q", got)
	}
	// H2 heading.
	if got := extractContentTitle("", "intro\n\n## My Heading\n\nbody", "fallback.md"); got != "My Heading" {
		t.Fatalf("h2: %q", got)
	}
	// H1 heading.
	if got := extractContentTitle("", "# top\nbody", "fallback.md"); got != "top" {
		t.Fatalf("h1: %q", got)
	}
	// Fallback.
	if got := extractContentTitle("", "no headings here", "fallback.md"); got != "fallback.md" {
		t.Fatalf("fallback: %q", got)
	}
	// Empty H1/H2 should not match.
	if got := extractContentTitle("", "# \n## \nbody", "fb"); got != "fb" {
		t.Fatalf("empty headings: %q", got)
	}
}

func TestSampleChunksForViz(t *testing.T) {
	var nilR *Repository
	got, err := nilR.SampleChunksForViz(context.Background(), "p", nil, false, 0)
	if got != nil || err != nil {
		t.Fatalf("nil-safe: %v %v", got, err)
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if got, _ := r.SampleChunksForViz(context.Background(), "", nil, false, 0); got != nil {
		t.Fatalf("empty project: %v", got)
	}

	// Without embedding, clamp upper limit.
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "content_size",
		}).AddRow("c1", "src.md", "T", "research", "verified", "scout", "preview text", 100))
	got, err = r.SampleChunksForViz(context.Background(), "p", nil, false, 5000)
	if err != nil || len(got) != 1 || got[0].DisplayTitle != "T" {
		t.Fatalf("non-embed: got %+v err %v", got, err)
	}

	// With embedding column.
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "content_size", "embedding",
		}).
			AddRow("c2", "src2.md", "", "spec", "unverified", "analyst", "# A heading\nbody", 200, "[0.1,0.2,0.3]").
			AddRow("c3", "src3.md", "", "spec", "unverified", "analyst", "noheading", 50, nil))
	got, err = r.SampleChunksForViz(context.Background(), "p", []string{"e1"}, true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].DisplayTitle != "A heading" || len(got[0].Embedding) != 3 {
		t.Fatalf("c2 wrong: %+v", got[0])
	}
	if got[1].DisplayTitle != "src3.md" || got[1].Embedding != nil {
		t.Fatalf("c3 wrong: %+v", got[1])
	}

	// Query error.
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("x"))
	if _, err := r.SampleChunksForViz(context.Background(), "p", nil, false, 0); err == nil {
		t.Fatal("want err")
	}
}

func TestParseVectorLiteral(t *testing.T) {
	if got := parseVectorLiteral(""); got != nil {
		t.Fatal("empty")
	}
	if got := parseVectorLiteral("[]"); got != nil {
		t.Fatal("empty brackets")
	}
	if got := parseVectorLiteral("nope"); got != nil {
		t.Fatal("no brackets")
	}
	got := parseVectorLiteral("[1.5, 2, -3]")
	if len(got) != 3 || got[0] != 1.5 || got[2] != -3 {
		t.Fatalf("got %v", got)
	}
	if got := parseVectorLiteral("[1, bad]"); got != nil {
		t.Fatalf("malformed: %v", got)
	}
}

func TestMarkVerifiedByArtifact(t *testing.T) {
	var nilR *Repository
	if err := nilR.MarkVerifiedByArtifact(context.Background(), "p", "a", "r"); err != nil {
		t.Fatal(err)
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Empty inputs → no-op.
	if err := r.MarkVerifiedByArtifact(context.Background(), "", "a", "r"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkVerifiedByArtifact(context.Background(), "p", "", "r"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a", "reviewer").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.MarkVerifiedByArtifact(context.Background(), "p", "a", "reviewer"); err != nil {
		t.Fatal(err)
	}
}

func TestSupersedeBySameSource(t *testing.T) {
	var nilR *Repository
	if n, err := nilR.SupersedeBySameSource(context.Background(), "", "", "", "", "", ""); n != 0 || err != nil {
		t.Fatal()
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Missing inputs → no-op (no expectation needed; sqlmock would fail).
	if n, _ := r.SupersedeBySameSource(context.Background(), "", "c", "s", "t", "a", ""); n != 0 {
		t.Fatal("p missing")
	}

	// New-style legacy name (no disambig suffix): the SQL passes
	// `s` as both the exact match AND the LIKE pattern slot. The
	// LIKE pattern is constructed as `{stem}-________-____{ext}`,
	// so for stem="s", ext="", the pattern is "s-________-____".
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "c", "t", "a", "s", "s-________-____", "epoch-1").
		WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := r.SupersedeBySameSource(context.Background(), "p", "c", "s", "t", "a", "epoch-1")
	if err != nil || n != 3 {
		t.Fatalf("got %d %v", n, err)
	}

	// Error.
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnError(errors.New("x"))
	if _, err := r.SupersedeBySameSource(context.Background(), "p", "c", "s", "t", "a", "epoch-1"); err == nil {
		t.Fatal("want err")
	}
}

// TestSupersedeBySameSource_DisambigAwareStem covers the case
// after 2026-05-16: the new chunk's source_name carries the
// `-YYYYMMDD-XXXX` disambig suffix. Supersession must strip the
// suffix to compute the stem, then issue a LIKE pattern that
// catches BOTH any prior disambig'd version (different suffix)
// AND the legacy un-suffixed form (chunks from before the
// disambig roll-out). The exact-match leg handles the legacy
// path; the LIKE leg handles cross-execution disambig variants.
func TestSupersedeBySameSource_DisambigAwareStem(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()

	// New chunk: research-20260516-c4d5.md (disambig'd).
	// Stem = "research", ext = ".md".
	// Legacy exact = "research.md"
	// LIKE pattern = "research-________-____.md"
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "research", "task-1", "art-new",
			"research.md",               // legacy exact match
			"research-________-____.md", // disambig LIKE pattern
			"epoch-1",                   // restore provenance (migration 89)
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	n, err := r.SupersedeBySameSource(context.Background(), "p", "research", "research-20260516-c4d5.md", "task-1", "art-new", "epoch-1")
	if err != nil || n != 2 {
		t.Fatalf("got %d %v", n, err)
	}
}

// TestSupersedeBySameSource_NoExtArtifact covers extension-less
// names (CHANGELOG, Makefile, .env). Stem == name when there's
// no ext, so the LIKE pattern is just `{stem}-________-____`.
func TestSupersedeBySameSource_NoExtArtifact(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()

	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "decision", "task-1", "art-new",
			"CHANGELOG",
			"CHANGELOG-________-____",
			"epoch-1",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	n, err := r.SupersedeBySameSource(context.Background(), "p", "decision", "CHANGELOG-20260516-c4d5", "task-1", "art-new", "epoch-1")
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
}

// TestSplitSourceNameForSupersede exercises the disambig-suffix
// detector. The shape recognizer is strict on purpose: a name
// like `report-stats-1ab2.md` that happens to be suffix-shaped
// at the wrong position must NOT have its `-1ab2` stripped — it
// would over-match unrelated files in the supersession LIKE.
func TestSplitSourceNameForSupersede(t *testing.T) {
	cases := []struct {
		name     string
		wantStem string
		wantExt  string
	}{
		// Plain names — no disambig suffix to strip.
		{"report.md", "report", ".md"},
		{"CHANGELOG", "CHANGELOG", ""},
		{".env", ".env", ""},
		// Already-disambig'd names — strip the suffix.
		{"report-20260516-1a2b.md", "report", ".md"},
		{"CHANGELOG-20260516-1a2b", "CHANGELOG", ""},
		{".env-20260516-1a2b", ".env", ""},
		{"research.tar-20260516-1a2b.gz", "research.tar", ".gz"},
		// Suffix-shaped but wrong slot positions — DON'T strip.
		// "report-stats-1ab2.md" → date slot is "stats" which
		// fails allDigits, so the suffix is rejected and the
		// whole name (minus ext) is returned as the stem.
		{"report-stats-1ab2.md", "report-stats-1ab2", ".md"},
		// "report-20260516-XYZW.md" → hex slot has invalid chars,
		// reject.
		{"report-20260516-XYZW.md", "report-20260516-XYZW", ".md"},
		// "rep-12345678-abcd.md" → exactly the right shape but
		// just a coincidence. Accept the strip — the operator
		// can't have it both ways; the suffix shape is the
		// canonical marker.
		{"rep-12345678-abcd.md", "rep", ".md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stem, ext := splitSourceNameForSupersede(c.name)
			if stem != c.wantStem || ext != c.wantExt {
				t.Errorf("splitSourceNameForSupersede(%q) = (%q, %q), want (%q, %q)", c.name, stem, ext, c.wantStem, c.wantExt)
			}
		})
	}
}

func TestStampEpochByArtifact(t *testing.T) {
	var nilR *Repository
	if err := nilR.StampEpochByArtifact(context.Background(), "", "", ""); err != nil {
		t.Fatal()
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if err := r.StampEpochByArtifact(context.Background(), "", "a", "e"); err != nil {
		t.Fatal()
	}
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a", "e").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.StampEpochByArtifact(context.Background(), "p", "a", "e"); err != nil {
		t.Fatal(err)
	}
}

func TestChunkExistsByHash(t *testing.T) {
	var nilR *Repository
	if got, err := nilR.ChunkExistsByHash(context.Background(), "p", "h"); got || err != nil {
		t.Fatal()
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if got, _ := r.ChunkExistsByHash(context.Background(), "", "h"); got {
		t.Fatal("p empty")
	}
	if got, _ := r.ChunkExistsByHash(context.Background(), "p", ""); got {
		t.Fatal("h empty")
	}
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("p", "h").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	got, err := r.ChunkExistsByHash(context.Background(), "p", "h")
	if err != nil || !got {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestPatchPolicyByArtifact(t *testing.T) {
	var nilR *Repository
	if err := nilR.PatchPolicyByArtifact(context.Background(), "p", "a", "c", 0.5, "r", "e", nil, ""); err != nil {
		t.Fatal()
	}
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	if err := r.PatchPolicyByArtifact(context.Background(), "", "a", "c", 0.5, "r", "e", nil, ""); err != nil {
		t.Fatal()
	}
	if err := r.PatchPolicyByArtifact(context.Background(), "p", "", "c", 0.5, "r", "e", nil, ""); err != nil {
		t.Fatal()
	}
	now := time.Now()
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a", "research", float32(0.5), "scout", "exec1", now, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.PatchPolicyByArtifact(context.Background(), "p", "a", "research", 0.5, "scout", "exec1", &now, ""); err != nil {
		t.Fatal(err)
	}
}

func TestVectorLiteral(t *testing.T) {
	if got := vectorLiteral(nil); got != "[]" {
		t.Fatalf("nil: %q", got)
	}
	got := vectorLiteral([]float32{1, 2.5, -3.25})
	if got != "[1,2.5,-3.25]" {
		t.Fatalf("got %q", got)
	}
}

// ---------- Hybrid + Keyword search ----------

func makeRR(ids []string, scores []float64) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "project_id", "task_id", "source_name", "content", "score"})
	for i, id := range ids {
		rows.AddRow(id, "p", "t", "s", "content", scores[i])
	}
	return rows
}

// TestScopeFilterSQL_StrictVsLenient pins the contract for the
// scope-filter helper. Both forms must:
//
//  1. short-circuit when the bound parameter is NULL (empty scope
//     means "no scope filter"),
//  2. accept exact-match on the scope token,
//  3. accept the cross-cutting "*" sentinel.
//
// They differ on only one clause: lenient mode also accepts NULL
// repo_scope (legacy chunks pre-migration-75), strict mode does
// not. Operator-facing surfaces want strict so the dropdown
// matches what the user picked; companion recall keeps lenient
// so host LLMs still see pre-migration deposits.
func TestScopeFilterSQL_StrictVsLenient(t *testing.T) {
	lenient := scopeFilterSQL(false, 7)
	strict := scopeFilterSQL(true, 7)

	for _, want := range []string{"$7::text IS NULL", "repo_scope = $7", "repo_scope = '*'"} {
		if !strings.Contains(lenient, want) {
			t.Errorf("lenient clause missing %q; got: %s", want, lenient)
		}
		if !strings.Contains(strict, want) {
			t.Errorf("strict clause missing %q; got: %s", want, strict)
		}
	}
	if !strings.Contains(lenient, "repo_scope IS NULL") {
		t.Errorf("lenient clause must include the IS NULL leak-through; got: %s", lenient)
	}
	// Strict mode keeps the $N::text IS NULL short-circuit (that's
	// how "empty filter" is expressed) but must not match
	// repo_scope IS NULL — that's the leak-through being removed.
	if strings.Contains(strict, "repo_scope IS NULL") {
		t.Errorf("strict clause must NOT include 'repo_scope IS NULL' (only the $N::text IS NULL short-circuit); got: %s", strict)
	}
}

// TestScopeFilterSQL_ParamIdxRendered makes sure the placeholder
// index is interpolated correctly — different callers use
// different indices (the hybrid epoch-aware query uses $8, the
// no-epoch sibling uses $7, the keyword fallbacks use $6/$7).
// A drift here would make the SQL bind a wrong parameter and
// silently match the wrong chunks.
func TestScopeFilterSQL_ParamIdxRendered(t *testing.T) {
	got := scopeFilterSQL(false, 12)
	if !strings.Contains(got, "$12::text IS NULL") || !strings.Contains(got, "repo_scope = $12") {
		t.Errorf("paramIdx=12 not rendered into the clause: %s", got)
	}
	if strings.Contains(got, "$7") || strings.Contains(got, "$8") {
		t.Errorf("clause leaked a different placeholder: %s", got)
	}
}

func TestHybridSearch_PgvectorOffFallsBackToKeyword(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// KeywordSearch query.
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.5}))
	got, err := r.HybridSearch(context.Background(), "p", []float32{0.1}, "q", 5)
	if err != nil || len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHybridSearch_NoVecFallsBackToKeyword(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := r.HybridSearch(context.Background(), "p", nil, "q", 5); err != nil {
		t.Fatal(err)
	}
}

func TestHybridSearch_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("WITH semantic AS")).
		WithArgs("p", "q", 5, "[0.1,0.2]").
		WillReturnRows(makeRR([]string{"c1", "c2"}, []float64{0.9, 0.5}))
	got, err := r.HybridSearch(context.Background(), "p", []float32{0.1, 0.2}, "q", 5)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHybridSearch_QueryErrorFallsBackToKeyword(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("WITH semantic AS")).
		WillReturnError(errors.New("pgvec gone"))
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := r.HybridSearch(context.Background(), "p", []float32{1}, "q", 5); err != nil {
		t.Fatal(err)
	}
}

func TestHybridSearchWithEpochs_DisabledDelegates(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := r.HybridSearchWithEpochs(context.Background(), "p", nil, "q", 5, nil, false, time.Time{}, time.Time{}, "", false); err != nil {
		t.Fatal(err)
	}
}

func TestHybridSearchWithEpochs_DisabledStillAppliesTemporalBounds(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 15, 23, 59, 59, 0, time.UTC)
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// 2026-05-29: keywordSearchTemporal (no-pgvector path) already
	// threaded repoScope at $6. The added scope arg is nil (empty
	// repoScope, lenient strict).
	mock.ExpectQuery(regexp.QuoteMeta("c.created_at >= $4")).
		WithArgs("p", "q", 5, from, to, nil).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.4}))
	got, err := r.HybridSearchWithEpochs(context.Background(), "p", nil, "q", 5, nil, false, from, to, "", false)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHybridSearchWithEpochs_TemporalKeywordErrorFallsBackToBoundedSubstring(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 15, 23, 59, 59, 0, time.UTC)
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("c.created_at >= $4")).
		WillReturnError(errors.New("tsvector down"))
	// 2026-05-29 audit fix: substringSearchTemporal now threads
	// repoScope at $6 via scopeFilterSQL so the ILIKE fallback no
	// longer silently leaks across scopes when the FTS tier fails.
	mock.ExpectQuery(regexp.QuoteMeta("c.content ILIKE '%' || $2 || '%'")).
		WithArgs("p", "q", 5, from, to, nil).
		WillReturnRows(makeRR([]string{"c1"}, []float64{1.0}))
	got, err := r.HybridSearchWithEpochs(context.Background(), "p", nil, "q", 5, nil, false, from, to, "", false)
	if err != nil || len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestHybridSearchWithEpochs_NoPgvectorTakesKeywordPath(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(regexp.QuoteMeta("c.epoch_id IS NULL")).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.4}))
	got, err := r.HybridSearchWithEpochs(context.Background(), "p", []float32{1}, "q", 5, []string{"e1"}, true, time.Time{}, time.Time{}, "", false)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHybridSearchWithEpochs_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("epoch_id = ANY($5")).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.7}))
	got, err := r.HybridSearchWithEpochs(context.Background(), "p", []float32{1}, "q", 5, []string{"e1"}, true, time.Time{}, time.Time{}, "", false)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHybridSearchWithEpochs_VecQueryFails(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("epoch_id = ANY($5")).
		WillReturnError(errors.New("x"))
	mock.ExpectQuery(regexp.QuoteMeta("c.epoch_id IS NULL")).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := r.HybridSearchWithEpochs(context.Background(), "p", []float32{1}, "q", 5, []string{"e1"}, true, time.Time{}, time.Time{}, "", false); err != nil {
		t.Fatal(err)
	}
}

func TestKeywordSearchWithEpochs_QueryErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("c.epoch_id IS NULL")).
		WillReturnError(errors.New("x"))
	mock.ExpectQuery(regexp.QuoteMeta("c.content ILIKE '%' || $2 || '%'")).
		WillReturnRows(makeRR([]string{"c1"}, []float64{1.0}))
	got, err := r.keywordSearchWithEpochs(context.Background(), "p", "q", 5, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("expected fallback hit, got %+v %v", got, err)
	}
}

func TestKeywordSearchWithEpochs_TierThreeAlsoFailsBubbles(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("c.epoch_id IS NULL")).
		WillReturnError(errors.New("tsvector down"))
	mock.ExpectQuery(regexp.QuoteMeta("c.content ILIKE '%' || $2 || '%'")).
		WillReturnError(errors.New("connection dropped"))
	if _, err := r.keywordSearchWithEpochs(context.Background(), "p", "q", 5, nil); err == nil {
		t.Fatal("expected error when both epoch keyword and bounded substring fallback fail")
	}
}

func TestPqStringArray_NilToEmpty(t *testing.T) {
	if got := pqStringArray(nil); got == nil {
		t.Fatal("want non-nil")
	}
	if got := pqStringArray([]string{"a"}); got == nil {
		t.Fatal("want non-nil")
	}
}

// TestKeywordSearch_FallsBackToSubstringOnTsvectorError — 2026.7.0
// F7 three-tier resilience: when the tsvector query errors (e.g.,
// extension dropped, malformed plainto_tsquery), the cascade
// degrades to substringSearch rather than bubbling an error. The
// caller never sees "memory unavailable" for a transient infra
// problem.
func TestKeywordSearch_FallsBackToSubstringOnTsvectorError(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("ts_rank").WillReturnError(errors.New("tsvector extension missing"))
	// 2026-05-29: substringSearch now takes repoScope (4th arg)
	// via scopeFilterSQL; KeywordSearch passes "" + lenient.
	mock.ExpectQuery("content ILIKE").
		WithArgs("p", "q", 5, nil).
		WillReturnRows(makeRR([]string{"c1"}, []float64{1.0}))
	got, err := r.KeywordSearch(context.Background(), "p", "q", 5)
	if err != nil {
		t.Fatalf("expected cascade to substringSearch to swallow the tsvector error, got: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("expected the substring hit to surface, got %+v", got)
	}
}

// TestKeywordSearch_TierThreeAlsoFailsBubbles — the cascade can't
// hide a fully-broken DB. When both tsvector AND substring queries
// fail (table unreachable / connection dropped), the error bubbles
// so the audit log captures the real problem rather than masking
// it as an empty result.
func TestKeywordSearch_TierThreeAlsoFailsBubbles(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("ts_rank").WillReturnError(errors.New("tsvector down"))
	mock.ExpectQuery("content ILIKE").WillReturnError(errors.New("connection dropped"))
	_, err := r.KeywordSearch(context.Background(), "p", "q", 5)
	if err == nil {
		t.Fatal("expected error when both tier-2 and tier-3 fail")
	}
}

// TestSubstringSearch_StrictScopeFiltersResults — regression
// test for the 2026-05-29 audit-agent finding: the tier-3 ILIKE
// fallback used to drop the repo_scope filter that the upper
// FTS/pgvector tiers applied, so a transient tsvector failure
// would silently leak cross-scope chunks into every scoped recall.
// Pin the SQL contract — scopeFilterSQL with strict=true at $4 —
// so a future refactor can't reintroduce the leak.
func TestSubstringSearch_StrictScopeFiltersResults(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// Match the scope clause shape exactly; sqlmock substring
	// match on the strict-scope SQL form rules out the lenient
	// "OR repo_scope IS NULL" fallback variant.
	mock.ExpectQuery(regexp.QuoteMeta("$4::text IS NULL OR c.repo_scope = $4 OR c.repo_scope = '*'")).
		WithArgs("p", "NVDA", 5, "github.com/me/repo").
		WillReturnRows(makeRR([]string{"c1"}, []float64{1.0}))
	got, err := r.substringSearch(context.Background(), "p", "NVDA", 5, "github.com/me/repo", true)
	if err != nil {
		t.Fatalf("substringSearch with strict scope: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("expected scoped hit, got %+v", got)
	}
}

// TestSubstringSearch_EmptyQueryReturnsNilWithoutHittingDB —
// pin the safety contract: a blank query string must NOT translate
// to ILIKE '%%' (which matches every row) and dump the whole
// project's memory to the caller.
func TestSubstringSearch_EmptyQueryReturnsNilWithoutHittingDB(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	got, err := r.substringSearch(context.Background(), "p", "   ", 5, "", false)
	if err != nil {
		t.Fatalf("empty query must not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("empty query must return nil results — got %+v", got)
	}
}

// TestSubstringSearch_HappyPath — the tier-3 SQL must actually
// project the same columns scanSearchResults expects + apply the
// project filter + ILIKE on content + the TTL guard.
func TestSubstringSearch_HappyPath(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	// 2026-05-29 audit fix: substringSearch now takes repoScope +
	// strictScope and threads them into the SQL via scopeFilterSQL.
	// Pass empty + lenient here to match the legacy unscoped wire
	// shape this test pinned. $4 binds NULL via nullableString.
	mock.ExpectQuery("content ILIKE").
		WithArgs("p", "NVDA", 5, nil).
		WillReturnRows(makeRR([]string{"c1", "c2"}, []float64{1.0, 1.0}))
	got, err := r.substringSearch(context.Background(), "p", "NVDA", 5, "", false)
	if err != nil {
		t.Fatalf("got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
}

func TestKeywordSearch_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.3}))
	got, err := r.KeywordSearch(context.Background(), "p", "q", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

// TestScanSearchResults_NineColumnShape — when the SQL projects the
// liveness columns, scanSearchResults must populate IsAlive +
// LastCheckedAt on SearchResult. The flag is what consuming agents
// rely on to prefer alive hits.
func TestScanSearchResults_NineColumnShape(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	checkedAt := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "project_id", "task_id", "source_name", "content", "score",
			"content_class", "is_alive", "last_checked_at",
		}).
			AddRow("c1", "p", "t", "src", "body", 0.5, "research", true, checkedAt).
			AddRow("c2", "p", "t", "src", "body", 0.4, "research", false, checkedAt).
			AddRow("c3", "p", "t", "src", "body", 0.3, "research", nil, nil))
	got, err := r.HybridSearch(context.Background(), "p", []float32{0.1}, "q", 5)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 results, got %d", len(got))
	}
	if got[0].IsAlive == nil || *got[0].IsAlive != true {
		t.Errorf("row 0: IsAlive want *true, got %v", got[0].IsAlive)
	}
	if got[1].IsAlive == nil || *got[1].IsAlive != false {
		t.Errorf("row 1: IsAlive want *false, got %v", got[1].IsAlive)
	}
	if got[2].IsAlive != nil {
		t.Errorf("row 2: IsAlive want nil (never checked), got %v", got[2].IsAlive)
	}
	if got[0].LastCheckedAt == nil || !got[0].LastCheckedAt.Equal(checkedAt) {
		t.Errorf("row 0: LastCheckedAt = %v, want %v", got[0].LastCheckedAt, checkedAt)
	}
}

// ---------- DLQ ----------

func TestDLQMove(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").
		WithArgs("c1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := r.DLQMove(context.Background(), "c1", "p", "embedding_failed", "err", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestDLQMove_BeginErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin().WillReturnError(errors.New("x"))
	if err := r.DLQMove(context.Background(), "c1", "p", "r", "e", time.Now()); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQMove_DeleteErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").WillReturnError(errors.New("x"))
	mock.ExpectRollback()
	if err := r.DLQMove(context.Background(), "c1", "p", "r", "e", time.Now()); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQMove_UpsertErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_dlq").WillReturnError(errors.New("x"))
	mock.ExpectRollback()
	if err := r.DLQMove(context.Background(), "c1", "p", "r", "e", time.Now()); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQPark(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectExec("UPDATE memory_embed_dlq").
		WithArgs("c1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.DLQPark(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
}

func dlqRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"chunk_id", "project_id", "reason", "last_error",
		"retry_count", "retry_after", "first_failed_at", "last_failed_at",
	}).AddRow("c1", "p", "embedding_failed", "boom", 1, time.Now(), time.Now(), time.Now())
}

func TestDLQReadyForRetry(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM memory_embed_dlq").
		WithArgs(50). // limit clamp 0 → 50
		WillReturnRows(dlqRows())
	got, err := r.DLQReadyForRetry(context.Background(), 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	// Error path.
	mock.ExpectQuery("FROM memory_embed_dlq").WillReturnError(errors.New("x"))
	if _, err := r.DLQReadyForRetry(context.Background(), 10); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQList_AllProjects(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM memory_embed_dlq").
		WithArgs(100). // limit default
		WillReturnRows(dlqRows())
	if _, err := r.DLQList(context.Background(), "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestDLQList_FilteredAndError(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM memory_embed_dlq").
		WithArgs("p", 25).
		WillReturnRows(dlqRows())
	if _, err := r.DLQList(context.Background(), "p", 25); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("FROM memory_embed_dlq").WillReturnError(errors.New("x"))
	if _, err := r.DLQList(context.Background(), "", 5); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQReplay_Empty(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	if n, err := r.DLQReplay(context.Background(), nil); n != 0 || err != nil {
		t.Fatal()
	}
}

func TestDLQReplay_Happy(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM memory_embed_dlq").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	n, err := r.DLQReplay(context.Background(), []string{"c1", "c2"})
	if err != nil || n != 2 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestDLQReplay_BeginErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin().WillReturnError(errors.New("x"))
	if _, err := r.DLQReplay(context.Background(), []string{"c1"}); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQReplay_InsertErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnError(errors.New("x"))
	mock.ExpectRollback()
	if _, err := r.DLQReplay(context.Background(), []string{"c1"}); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQReplay_DeleteErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM memory_embed_dlq").WillReturnError(errors.New("x"))
	mock.ExpectRollback()
	if _, err := r.DLQReplay(context.Background(), []string{"c1"}); err == nil {
		t.Fatal("want err")
	}
}

func TestDLQReplay_CommitErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM memory_embed_dlq").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("x"))
	if _, err := r.DLQReplay(context.Background(), []string{"c1"}); err == nil {
		t.Fatal("want err")
	}
}

func TestStats(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "chunks_total", "chunks_embedded", "queue_depth"}).
			AddRow("p1", 100, 80, 5))
	got, err := r.Stats(context.Background())
	if err != nil || len(got) != 1 || got[0].ChunksEmbedded != 80 {
		t.Fatalf("got %+v %v", got, err)
	}
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("x"))
	if _, err := r.Stats(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

func TestQueryState(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "chunks_total", "queue_depth"}).
			AddRow("p1", 10, 1))
	got, err := r.QueryState(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errors.New("x"))
	if _, err := r.QueryState(context.Background()); err == nil {
		t.Fatal("want err")
	}
}

// TestSupersedeBySameSource_RecordsRestoreProvenance pins the
// migration-89 statement shape: the supersede UPDATE must capture the
// pre-update validation_status into pre_supersede_status and stamp
// the causing epoch into superseded_in_epoch — that provenance is
// what makes a later rollback able to restore the prior version
// (2026-06-04 bug-sweep critical finding). Fails against the pre-fix
// statement, which set validation_status only.
func TestSupersedeBySameSource_RecordsRestoreProvenance(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()

	mock.ExpectExec(`(?s)UPDATE project_memory_chunks.*SET validation_status\s+= 'superseded'.*pre_supersede_status = validation_status.*superseded_in_epoch\s+= NULLIF\(\$7, ''\)`).
		WithArgs("p", "c", "t", "a", "s", "s-________-____", "epoch-9").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := r.SupersedeBySameSource(context.Background(), "p", "c", "s", "t", "a", "epoch-9")
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("provenance capture missing from supersede UPDATE: %v", err)
	}
}
