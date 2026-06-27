package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// recordingAudit captures the last Record() call so tests can assert
// what the searcher writes.
type recordingAudit struct {
	mu       sync.Mutex
	last     *persistence.MemoryRetrievalAudit
	err      error
	recorded int
}

func (r *recordingAudit) Record(_ context.Context, a *persistence.MemoryRetrievalAudit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = a
	r.recorded++
	return r.err
}

func (r *recordingAudit) FeedbackStats(context.Context, string, time.Time) (*persistence.MemoryFeedbackStats, error) {
	return nil, nil
}

func (r *recordingAudit) UnretrievedChunkIDs(context.Context, string, time.Time, int) ([]string, error) {
	return nil, nil
}

func (r *recordingAudit) List(context.Context, persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	return nil, nil
}

func TestNewSearcher_AndSetters(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	if s.repo != r {
		t.Fatal("repo wire")
	}

	// Setters are nil-safe.
	var nilS *Searcher
	nilS.SetAuditRepo(&recordingAudit{})
	nilS.SetLogger(zerolog.Nop())
	nilS.SetEpochSource(nil)

	audit := &recordingAudit{}
	s.SetAuditRepo(audit)
	if s.auditRepo == nil {
		t.Fatal("audit not set")
	}
	s.SetLogger(zerolog.Nop())
	s.SetEpochSource(func(context.Context, string) ([]string, error) { return []string{"e"}, nil })
	if s.epochSource == nil {
		t.Fatal("epoch not set")
	}
	s.setMetrics(freshMetrics())
	if s.metrics == nil {
		t.Fatal("metrics not set")
	}
}

func TestSearch_KeywordOnly_NoEmbedder(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.setMetrics(freshMetrics())

	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 10).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.5}))
	got, err := s.Search(context.Background(), "p", "q", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestSearch_EpochSourceErrorDegrades(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetEpochSource(func(context.Context, string) ([]string, error) {
		return nil, errors.New("epoch boom")
	})

	mock.ExpectQuery("SELECT EXISTS.*pg_extension").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery("c.epoch_id IS NULL").
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := s.Search(context.Background(), "p", "q", 5); err != nil {
		t.Fatal(err)
	}
}

func TestSearch_AuditWrittenWithRetrievalContext(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	audit := &recordingAudit{}
	s.SetAuditRepo(audit)

	mock.ExpectQuery("ts_rank").
		WillReturnRows(makeRR([]string{"c1", "c2"}, []float64{0.5, 0.4}))

	rc := &RetrievalContext{TaskID: "T", ExecutionID: "E", StepID: "S", Role: "researcher"}
	ctx := WithRetrievalContext(context.Background(), rc)
	got, err := s.Search(ctx, "p", "q", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	if audit.recorded != 1 || audit.last == nil {
		t.Fatalf("audit not recorded: %+v", audit)
	}
	if audit.last.TaskID == nil || *audit.last.TaskID != "T" {
		t.Fatalf("task: %+v", audit.last)
	}
	if audit.last.Role == nil || *audit.last.Role != "researcher" {
		t.Fatalf("role: %+v", audit.last)
	}
	if len(audit.last.ChunkIDs) != 2 {
		t.Fatalf("chunkids: %+v", audit.last.ChunkIDs)
	}
}

func TestSearch_AuditWriteErrorSwallowed(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetAuditRepo(&recordingAudit{err: errors.New("audit down")})

	mock.ExpectQuery("ts_rank").
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.5}))
	if _, err := s.Search(context.Background(), "p", "q", 5); err != nil {
		t.Fatalf("audit failure must not bubble: %v", err)
	}
}

// stubReranker reverses the input slice — gives the searcher integration test
// a deterministic, no-LLM way to verify the rerank step ran.
type stubReranker struct {
	called bool
	err    error
}

func (s *stubReranker) Rerank(_ context.Context, _ string, in []SearchResult) ([]SearchResult, error) {
	s.called = true
	if s.err != nil {
		return nil, s.err
	}
	out := make([]SearchResult, len(in))
	for i, r := range in {
		out[len(in)-1-i] = r
	}
	return out, nil
}

func TestSetReranker_NilSafe(t *testing.T) {
	var nilS *Searcher
	nilS.SetReranker(NoopReranker{})
	r, _, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{})
	if s.reranker == nil {
		t.Fatal("reranker not set")
	}
}

func TestSearch_RerankAppliedAndTruncated(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	rr := &stubReranker{}
	s.SetReranker(rr)
	s.setMetrics(freshMetrics())

	// Reranker is wired + opted in → searcher fetches 3*limit = 15.
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 15).
		WillReturnRows(makeRR([]string{"a", "b", "c"}, []float64{0.9, 0.7, 0.5}))
	out, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rr.called {
		t.Fatal("reranker not invoked")
	}
	// stubReranker reversed: c, b, a.
	if out[0].ChunkID != "c" || out[2].ChunkID != "a" {
		t.Fatalf("reorder: %+v", out)
	}
}

func TestSearch_RerankFetchLimitClampedAt60(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetReranker(&stubReranker{})

	// limit=30 → 3x=90, clamped to 60 (rerank opted in).
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 60).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 30, Rerank: true}); err != nil {
		t.Fatal(err)
	}
}

// TestSearch_NoRerankByDefault asserts the interactive path stays on RRF: with
// a reranker wired but Rerank not set, the searcher neither widens the fetch
// nor calls the reranker.
func TestSearch_NoRerankByDefault(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	rr := &stubReranker{}
	s.SetReranker(rr)
	s.setMetrics(freshMetrics())

	// No Rerank opt-in → fetch == limit (5), reranker untouched.
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "q", 5).
		WillReturnRows(makeRR([]string{"a", "b", "c"}, []float64{0.9, 0.7, 0.5}))
	out, err := s.Search(context.Background(), "p", "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if rr.called {
		t.Fatal("reranker must NOT be invoked without Rerank opt-in")
	}
	// RRF order preserved (not reversed by the stub).
	if out[0].ChunkID != "a" {
		t.Fatalf("expected RRF order, got %+v", out)
	}
}

func TestSearch_RerankErrorKeepsRRFOrder(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetLogger(zerolog.Nop())
	s.SetReranker(&stubReranker{err: errors.New("rerank down")})
	s.setMetrics(freshMetrics())

	mock.ExpectQuery("ts_rank").
		WillReturnRows(makeRR([]string{"a", "b"}, []float64{0.9, 0.5}))
	out, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ChunkID != "a" || out[1].ChunkID != "b" {
		t.Fatalf("must keep RRF on rerank err: %+v", out)
	}
}

func TestSearch_RerankSkippedOnSingleResult(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	rr := &stubReranker{}
	s.SetReranker(rr)

	mock.ExpectQuery("ts_rank").
		WillReturnRows(makeRR([]string{"only"}, []float64{0.5}))
	_, _ = s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true})
	if rr.called {
		t.Fatal("reranker should be skipped for <2 results")
	}
}

func TestSearch_MMRApplied(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetMMRLambda(0.5)

	// Three near-duplicate top results — MMR should reorder.
	rows := sqlmock.NewRows([]string{"id", "project_id", "task_id", "source_name", "content", "score"}).
		AddRow("a", "p", "t", "s", "deploy script for cluster", 0.9).
		AddRow("b", "p", "t", "s", "deploy script for cluster copy", 0.8).
		AddRow("c", "p", "t", "s", "deploy script for cluster again", 0.7).
		AddRow("d", "p", "t", "s", "incident response runbook unrelated", 0.6)
	mock.ExpectQuery("ts_rank").WillReturnRows(rows)

	out, err := s.Search(context.Background(), "p", "q", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("lost results: %+v", out)
	}
	// d should be promoted past at least one of b/c.
	posD, posB, posC := indexOf(out, "d"), indexOf(out, "b"), indexOf(out, "c")
	if posD >= posB && posD >= posC {
		t.Fatalf("MMR didn't diversify: %+v", out)
	}
}

func TestSearch_MMRSkippedBelowThreshold(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetMMRLambda(0.5)
	// 2 results → applyMMR pass-through; we just verify no error.
	mock.ExpectQuery("ts_rank").
		WillReturnRows(makeRR([]string{"a", "b"}, []float64{0.9, 0.5}))
	out, err := s.Search(context.Background(), "p", "q", 5)
	if err != nil || len(out) != 2 {
		t.Fatalf("got %v %v", out, err)
	}
}

type stubExpander struct {
	terms []string
	gotQ  string
}

func (s *stubExpander) Expand(_ context.Context, _, q string) []string {
	s.gotQ = q
	return s.terms
}

func TestSearch_QueryExpanderWidensKeywordSide(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	exp := &stubExpander{terms: []string{"kubernetes", "container"}}
	s.SetQueryExpander(exp)

	// Searcher should call the keyword SQL with the widened query string.
	mock.ExpectQuery("ts_rank").
		WithArgs("p", "deploy kubernetes container", 10).
		WillReturnRows(makeRR([]string{"c1"}, []float64{0.5}))

	if _, err := s.Search(context.Background(), "p", "deploy", 10); err != nil {
		t.Fatal(err)
	}
	if exp.gotQ != "deploy" {
		t.Fatalf("expander should see ORIGINAL query, got %q", exp.gotQ)
	}
}

func TestSearch_QueryExpanderEmptyExpansionIsNoOp(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	s.SetQueryExpander(&stubExpander{terms: nil})

	mock.ExpectQuery("ts_rank").
		WithArgs("p", "deploy", 10).
		WillReturnRows(makeRR([]string{}, []float64{}))
	if _, err := s.Search(context.Background(), "p", "deploy", 10); err != nil {
		t.Fatal(err)
	}
}

func TestSetQueryExpander_NilSafe(t *testing.T) {
	var nilS *Searcher
	nilS.SetQueryExpander(&stubExpander{})
}

func TestSearch_RepoErrorPropagatesNoAudit(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	s := NewSearcher(Config{}, r, nil)
	audit := &recordingAudit{}
	s.SetAuditRepo(audit)
	s.setMetrics(freshMetrics())

	mock.ExpectQuery("ts_rank").WillReturnError(errors.New("db boom"))
	if _, err := s.Search(context.Background(), "p", "q", 5); err == nil {
		t.Fatal("want err")
	}
	if audit.recorded != 0 {
		t.Fatalf("audit should not be recorded on error")
	}
}
