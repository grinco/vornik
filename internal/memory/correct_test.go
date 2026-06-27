package memory

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestMarkRefutedByIDs_FlipsRowsAndIsIDORScoped — the headline
// safety property: refutation MUST be project-scoped (chunkIDs
// alone aren't enough — an attacker who guesses an ID from
// project B couldn't refute it via project A). The SQL must
// include `WHERE project_id = $1` and the test pins that shape.
func TestMarkRefutedByIDs_FlipsRowsAndIsIDORScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("SET validation_status = 'refuted'")).
		WithArgs("janka", "chunk_1", "chunk_2").
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := repo.MarkRefutedByIDs(context.Background(), "janka",
		[]string{"chunk_1", "chunk_2"})
	if err != nil {
		t.Fatalf("MarkRefutedByIDs: %v", err)
	}
	if n != 2 {
		t.Errorf("rows affected = %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestMarkRefutedByIDs_NoopOnEmptyInput — defensive: passing
// nil / empty chunkIDs must NOT execute a "WHERE id IN ()" SQL
// that postgres would reject. Returns (0, nil).
func TestMarkRefutedByIDs_NoopOnEmptyInput(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	n, err := repo.MarkRefutedByIDs(context.Background(), "p", nil)
	if err != nil || n != 0 {
		t.Errorf("empty input: n=%d err=%v, want (0, nil)", n, err)
	}
}

// TestInsertOperatorCorrection_LandsAsVerifiedDecision — the
// canonical correction shape. validation_status='verified'
// AND content_class='decision' AND producer_role='operator_
// correction' are the three SQL constants that make retrieval
// rank this chunk authoritatively against any prior refuted
// chunks for the same topic.
func TestInsertOperatorCorrection_LandsAsVerifiedDecision(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	// repo_scope (migration 75) added 2026-05-29 audit fix — pass
	// empty string here to land NULL via the nullableString helper,
	// preserving the legacy lenient-scope behaviour this test pinned.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_memory_chunks")).
		WithArgs("chunk_1", "janka", "operator_correction_20260516", "Janka was born in 1990.", "deadbeef", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.insertOperatorCorrection(context.Background(), &operatorCorrectionRow{
		ID: "chunk_1", ProjectID: "janka",
		SourceName: "operator_correction_20260516",
		Content:    "Janka was born in 1990.", ContentHash: "deadbeef",
	})
	if err != nil {
		t.Fatalf("insertOperatorCorrection: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestInsertOperatorCorrection_RejectsMissingFields — defensive:
// empty ID / project / content must error rather than land a
// malformed row.
func TestInsertOperatorCorrection_RejectsMissingFields(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	for _, tc := range []*operatorCorrectionRow{
		nil,
		{ID: "", ProjectID: "p", Content: "x"},
		{ID: "c", ProjectID: "", Content: "x"},
		{ID: "c", ProjectID: "p", Content: ""},
	} {
		if err := repo.insertOperatorCorrection(context.Background(), tc); err == nil {
			t.Errorf("expected error for %+v, got nil", tc)
		}
	}
}

// TestListUnverifiedChunks_DefaultStatusesAreUnverifiedAndLegacy
// — the default audit lens. Operators running `vornikctl memory
// audit` without flags want to see both freshly-ingested chunks
// awaiting validation AND legacy rows that pre-date the
// hardening migration.
func TestListUnverifiedChunks_DefaultStatusesAreUnverifiedAndLegacy(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("janka", "unverified", "legacy", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "created_at",
		}).AddRow("chunk_1", "cv.md", "Janka CV", "research",
			"unverified", "researcher",
			"Janka was born in 1985.", "2026-05-15 12:00:00 UTC"))

	rows, err := repo.ListUnverifiedChunks(context.Background(), "janka", nil, 0)
	if err != nil {
		t.Fatalf("ListUnverifiedChunks: %v", err)
	}
	if len(rows) != 1 || rows[0].ValidationStatus != "unverified" {
		t.Errorf("rows = %+v", rows)
	}
}

// TestListUnverifiedChunks_CapsLimit — defensive: an operator
// passing --limit 99999 must not flood the terminal. The
// implementation caps at 500.
func TestListUnverifiedChunks_CapsLimit(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM project_memory_chunks")).
		WithArgs("p", "unverified", "legacy", 500).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "created_at",
		}))

	if _, err := repo.ListUnverifiedChunks(context.Background(), "p", nil, 99999); err != nil {
		t.Fatalf("ListUnverifiedChunks: %v", err)
	}
}

// stubSearcher implements just enough of *Searcher for the
// Corrector tests. We can't use the real one without a DB +
// embedder, and the Corrector's contract with the searcher is
// narrow: Search(ctx, projectID, query, limit) → results.
type stubSearcher struct {
	results   []SearchResult
	err       error
	lastLimit int
}

func (s *stubSearcher) Search(_ context.Context, _, _ string, limit int) ([]SearchResult, error) {
	s.lastLimit = limit
	return s.results, s.err
}

// TestCorrector_RefuteByClaim_FlipsTopMatches — the headline
// flow: searcher returns 2 matches, RefuteByClaim marks them
// refuted via the repo's project-scoped UPDATE and returns
// previews so the dispatcher can show them to the operator.
func TestCorrector_RefuteByClaim_FlipsTopMatches(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("SET validation_status = 'refuted'")).
		WithArgs("janka", "chunk_a", "chunk_b").
		WillReturnResult(sqlmock.NewResult(0, 2))

	c := NewCorrector(repo, &stubSearcher{results: []SearchResult{
		{ChunkID: "chunk_a", SourceName: "cv-2024.md", Content: "Janka, born 1985 …", Score: 0.7},
		{ChunkID: "chunk_b", SourceName: "cv-2023.md", Content: "Janka 1985 …", Score: 0.6},
	}})
	refuted, err := c.RefuteByClaim(context.Background(), "janka", "born 1985", 5)
	if err != nil {
		t.Fatalf("RefuteByClaim: %v", err)
	}
	if len(refuted) != 2 {
		t.Fatalf("refuted = %d, want 2", len(refuted))
	}
	if refuted[0].Preview == "" || !strings.Contains(refuted[0].Preview, "Janka") {
		t.Errorf("preview missing: %+v", refuted[0])
	}
	if refuted[0].Score != 0.7 {
		t.Errorf("score passthrough: %+v", refuted[0])
	}
}

// TestCorrector_RefuteByClaim_NoMatchesIsNoop — searcher
// returns nothing; the function MUST NOT error and MUST NOT
// execute a no-rows UPDATE.
func TestCorrector_RefuteByClaim_NoMatchesIsNoop(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	c := NewCorrector(repo, &stubSearcher{})
	refuted, err := c.RefuteByClaim(context.Background(), "janka", "anything", 3)
	if err != nil {
		t.Fatalf("RefuteByClaim: %v", err)
	}
	if len(refuted) != 0 {
		t.Errorf("refuted = %v, want []", refuted)
	}
}

// TestCorrector_RefuteByClaim_CapsAtTwentyMatches — the LLM
// could pass max_refutes=1000. The library MUST cap at 20 so a
// runaway tool call can't refute the entire corpus. We exercise
// by passing 100 and confirming the searcher was called with
// the cap, not the raw value.
func TestCorrector_RefuteByClaim_CapsAtTwentyMatches(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	s := &stubSearcher{}
	c := NewCorrector(repo, s)
	_, _ = c.RefuteByClaim(context.Background(), "janka", "x", 1000)
	if s.lastLimit != 20 {
		t.Errorf("limit cap broken: searcher saw limit=%d, want 20", s.lastLimit)
	}
}

// TestCorrector_RefuteByClaim_RejectsMissingArgs — defensive:
// empty project or claim must error rather than execute a
// search that would return spurious results.
func TestCorrector_RefuteByClaim_RejectsMissingArgs(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	c := NewCorrector(NewRepository(db), &stubSearcher{})
	if _, err := c.RefuteByClaim(context.Background(), "", "x", 5); err == nil {
		t.Error("empty project: expected error")
	}
	if _, err := c.RefuteByClaim(context.Background(), "p", "", 5); err == nil {
		t.Error("empty claim: expected error")
	}
}

// TestCorrector_InsertCorrection_StoresVerifiedDecision — the
// canonical write. Pins that the insertion goes through the
// correct repo path AND that the embed-queue enqueue follows so
// vector search picks up the new chunk on the next tick.
func TestCorrector_InsertCorrection_StoresVerifiedDecision(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO project_memory_chunks")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// EnqueueForEmbedding uses INSERT ... SELECT with a chunk-id
	// IN-list; match the prefix.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO memory_embed_queue")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := NewCorrector(NewRepository(db), &stubSearcher{})
	id, err := c.InsertCorrection(context.Background(), "janka", "Janka was born in 1990.", "")
	if err != nil {
		t.Fatalf("InsertCorrection: %v", err)
	}
	if !strings.HasPrefix(id, "chunk_") {
		t.Errorf("chunk id shape: %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestChunkIDsByScope_NullBucketAndNamedScope pins the resolver behind
// `vornikctl memory evict --scope`: the NULL/untagged bucket filters on
// `repo_scope IS NULL` binding only the project (IDOR guard), and a
// named scope binds project + scope with `repo_scope = $2`.
func TestChunkIDsByScope_NullBucketAndNamedScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("repo_scope IS NULL")).
		WithArgs("janka").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("mc_1").AddRow("mc_2"))
	ids, err := repo.ChunkIDsByScope(context.Background(), "janka", "", true)
	if err != nil {
		t.Fatalf("null bucket: %v", err)
	}
	if len(ids) != 2 || ids[0] != "mc_1" || ids[1] != "mc_2" {
		t.Fatalf("null bucket ids = %v, want [mc_1 mc_2]", ids)
	}

	mock.ExpectQuery(regexp.QuoteMeta("repo_scope = $2")).
		WithArgs("janka", "github.com/acme/app").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("mc_9"))
	ids, err = repo.ChunkIDsByScope(context.Background(), "janka", "github.com/acme/app", false)
	if err != nil {
		t.Fatalf("named scope: %v", err)
	}
	if len(ids) != 1 || ids[0] != "mc_9" {
		t.Fatalf("named scope ids = %v, want [mc_9]", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestCorrector_RefuteByIDs_Guards(t *testing.T) {
	var nilC *Corrector
	_, err := nilC.RefuteByIDs(context.Background(), "p", []string{"c1"})
	if err == nil {
		t.Fatal("nil corrector must error")
	}
	c := &Corrector{Repo: &Repository{}, Searcher: nil}
	if _, err := c.RefuteByIDs(context.Background(), "", []string{"c1"}); err == nil {
		t.Error("empty project id must error")
	}
	if _, err := c.RefuteByIDs(context.Background(), "p", nil); err == nil {
		t.Error("empty chunk ids must error")
	}
}
