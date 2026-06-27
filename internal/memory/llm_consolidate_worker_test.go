package memory

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
)

// sqlMockNoRowsErr matches what sqlmock returns when a QueryRow
// path finds zero rows — sql.ErrNoRows. Repository.GetGist maps
// that to ErrGistNotFound, which the worker treats as "skip".
var sqlMockNoRowsErr = sql.ErrNoRows

// llmTickHappyPath: two projects, both have existing gists + chunks
// — narrative writer fires for each, narrative column gets
// UPDATEd. Tick outcome: progressed.
func TestLLMConsolidateWorker_TickHappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: "narrative for proj-a"},
		{content: "narrative for proj-b"},
	}}
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(fp, ""),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"proj-a", "proj-b"}},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}

	// proj-a: GetGist returns a populated row.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("proj-a").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("proj-a", `[{"Term":"ibkr","Count":42}]`, 100, time.Now(), 5, nil, nil, nil))
	// proj-a chunk sample.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("proj-a", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("chunk one"))
	// UpsertNarrative for proj-a.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WithArgs("proj-a", "narrative for proj-a", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// proj-b: same shape.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("proj-b").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("proj-b", `[{"Term":"gmail","Count":7}]`, 30, time.Now(), 2, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("proj-b", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("chunk two"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WithArgs("proj-b", "narrative for proj-b", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestLLMConsolidateWorker_SkipsProjectWithoutGist: when a project
// has no row in project_gists (the LLM-free worker hasn't fired
// yet), processOne MUST treat it as a no-op — no LLM call, no
// failure counter.
func TestLLMConsolidateWorker_SkipsProjectWithoutGist(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	fp := &titlerFakeProvider{replies: []titlerReply{{content: "should not fire"}}}
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(fp, ""),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"orphan"}},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}
	// GetGist returns sql.ErrNoRows → ErrGistNotFound → skipped.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("orphan").
		WillReturnError(sqlmockNoRows)

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("writer was called on orphan project (calls=%d) — should be skipped", fp.calls.Load())
	}
}

// TestLLMConsolidateWorker_OneProjectFailsOtherSucceeds — same
// failure-isolation contract as the LLM-free tier.
func TestLLMConsolidateWorker_OneProjectFailsOtherSucceeds(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: "good-project narrative"},
	}}
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(fp, ""),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"bad-project", "good-project"}},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}

	// bad-project: GetGist errors with a real DB error.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("bad-project").
		WillReturnError(errors.New("connection refused"))

	// good-project: full happy path.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("good-project").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("good-project", `[{"Term":"x","Count":1}]`, 10, time.Now(), 1, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("good-project", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("y"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_gists")).
		WithArgs("good-project", "good-project narrative", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestLLMConsolidateWorker_EmptyNarrativePreservesExisting: when
// the writer returns "" without error (LLM said "too thin"), the
// worker MUST NOT clobber the existing narrative column.
func TestLLMConsolidateWorker_EmptyNarrativePreservesExisting(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)

	// Empty content → cleanNarrative → empty → Writer returns ""
	// without error. The worker should skip the UPDATE.
	fp := &titlerFakeProvider{replies: []titlerReply{
		{content: "   "},
	}}
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(fp, ""),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"proj-x"}},
		Interval: time.Hour,
		Logger:   zerolog.Nop(),
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("proj-x").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("proj-x", `[{"Term":"x","Count":1}]`, 5, time.Now(), 1, "old narrative", "old-model", time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("proj-x", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("y"))
	// Note: NO UPDATE expectation — the test will fail if the
	// worker tries to clobber the existing narrative with "".

	// Writer.Write returns "" + an error in this case (empty
	// cleaned response). The worker logs and counts it as
	// errored. That's acceptable behaviour — the existing
	// narrative is preserved on disk either way.
	// (If you re-design Write to return "" with nil error for the
	// "too thin" case, this test exercises the no-UPDATE branch.)
	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestLLMConsolidateWorker_DisabledByInterval — Interval <= 0
// returns immediately.
func TestLLMConsolidateWorker_DisabledByInterval(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(&titlerFakeProvider{}, ""),
		Repo:     repo,
		Projects: &stubProjectLister{ids: []string{"p"}},
		Interval: 0,
		Logger:   zerolog.Nop(),
	}
	done := make(chan struct{})
	go func() { w.Run(context.Background()); close(done) }()
	select {
	case <-done:
		// Returned immediately as required.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("worker did not return on Interval=0")
	}
}

func TestLLMConsolidateWorker_NilGuardsExitCleanly(t *testing.T) {
	// Writer / Repo / Projects nil should return immediately.
	(&LLMConsolidateWorker{Interval: time.Second}).Run(context.Background())
}

// sqlmockNoRows is a sentinel sqlmock-friendly error matching what
// Repository.GetGist sees when the row is missing. sqlmock's
// QueryRow path returns sql.ErrNoRows when zero rows match; we
// reuse that exact value here so the test triggers the
// ErrGistNotFound branch deterministically.
var sqlmockNoRows = func() error {
	// The Go sql package returns sql.ErrNoRows; importing it via
	// the database/sql alias keeps this file slim.
	return sqlMockNoRowsErr
}()
