package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// stubMemorySearcher satisfies MemorySearcher with operator-controlled
// return values so the handler is exercised without a real pgvector
// stack.
type stubMemorySearcher struct {
	results []MemorySearchResult
	err     error
	lastQ   string
	lastLim int
	// B-6: capture the scope param the handler threaded through so
	// tests can pin "empty scope means project-wide search" and
	// "non-empty scope reaches the searcher verbatim".
	lastScope  string
	scopes     []MemoryRepoScope
	scopesErr  error
	scopesCall int
	// B-15: capture the ctx so tests can assert the handler stamped
	// memory.RetrievalContext before calling the searcher.
	lastCtx context.Context
}

func (s *stubMemorySearcher) Search(ctx context.Context, _, query string, limit int) ([]MemorySearchResult, error) {
	s.lastQ = query
	s.lastLim = limit
	s.lastScope = ""
	s.lastCtx = ctx
	if s.err != nil {
		return nil, s.err
	}
	// Return a shallow copy so the test can assert on per-result
	// fields (e.g. RepoScope) without losing them to MemoryProject's
	// snippet-truncation pass which mutates Content in-place.
	out := make([]MemorySearchResult, len(s.results))
	copy(out, s.results)
	return out, nil
}

func (s *stubMemorySearcher) SearchWithScope(ctx context.Context, _, query string, limit int, repoScope string) ([]MemorySearchResult, error) {
	s.lastQ = query
	s.lastLim = limit
	s.lastScope = repoScope
	s.lastCtx = ctx
	if s.err != nil {
		return nil, s.err
	}
	out := make([]MemorySearchResult, len(s.results))
	copy(out, s.results)
	return out, nil
}

func (s *stubMemorySearcher) ListRepoScopes(_ context.Context, _ string) ([]MemoryRepoScope, error) {
	s.scopesCall++
	if s.scopesErr != nil {
		return nil, s.scopesErr
	}
	return s.scopes, nil
}

func TestMemorySearchAction_NoSearcherConfigured(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when searcher unset, got %d", rr.Code)
	}
}

func TestMemorySearchAction_MissingQuery(t *testing.T) {
	srv := NewServer(WithMemorySearcher(&stubMemorySearcher{}))
	req := httptest.NewRequest("GET", "/memory/proj/search", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when q missing, got %d", rr.Code)
	}
}

func TestMemorySearchAction_ReturnsJSON(t *testing.T) {
	stub := &stubMemorySearcher{
		results: []MemorySearchResult{
			{ChunkID: "abc123", ProjectID: "proj", TaskID: "task1", SourceName: "research.md", Content: "hello world", Score: 0.42},
			{ChunkID: "def456", ProjectID: "proj", SourceName: "notes.md", Content: "another hit", Score: 0.31},
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello&limit=5", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Query   string               `json:"query"`
		Limit   int                  `json:"limit"`
		Count   int                  `json:"count"`
		Results []MemorySearchResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, rr.Body.String())
	}
	if body.Query != "hello" || body.Limit != 5 || body.Count != 2 {
		t.Errorf("response envelope wrong: query=%q limit=%d count=%d", body.Query, body.Limit, body.Count)
	}
	if len(body.Results) != 2 || body.Results[0].ChunkID != "abc123" {
		t.Errorf("results slice wrong: %+v", body.Results)
	}
	if stub.lastQ != "hello" || stub.lastLim != 5 {
		t.Errorf("searcher invoked with wrong args: q=%q limit=%d", stub.lastQ, stub.lastLim)
	}
}

func TestMemorySearchAction_SnippetTruncation(t *testing.T) {
	big := strings.Repeat("x", 2000)
	stub := &stubMemorySearcher{
		results: []MemorySearchResult{
			{ChunkID: "long", ProjectID: "proj", SourceName: "big.md", Content: big, Score: 0.5},
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=x", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var body struct {
		Results []MemorySearchResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(body.Results))
	}
	// 800-char cap + 1-rune ellipsis (3 bytes in UTF-8).
	if got := len(body.Results[0].Content); got > 900 {
		t.Errorf("content not truncated: %d bytes", got)
	}
	if !strings.HasSuffix(body.Results[0].Content, "…") {
		t.Errorf("truncation marker missing; got tail %q", body.Results[0].Content[max(0, len(body.Results[0].Content)-10):])
	}
}

// TestMemorySearchAction_ScopeParamReachesSearcher pins B-6: when
// the client passes ?repo_scope=<token>, the handler must forward
// the exact value into SearchWithScope so the underlying
// searcher's SQL filter (`AND repo_scope = $N OR scope = '*' OR
// scope IS NULL`) can scope the result set.
func TestMemorySearchAction_ScopeParamReachesSearcher(t *testing.T) {
	stub := &stubMemorySearcher{}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello&repo_scope=github.com/x/y", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stub.lastScope != "github.com/x/y" {
		t.Errorf("expected scope to reach searcher, got %q", stub.lastScope)
	}
}

// TestMemorySearchAction_EmptyScopeIsProjectWide pins the
// "no scope filter" branch: empty / missing repo_scope must
// behave like the legacy project-wide search. The handler still
// calls SearchWithScope (single path), but with "" so the
// searcher's SQL gates take the no-filter branch.
func TestMemorySearchAction_EmptyScopeIsProjectWide(t *testing.T) {
	stub := &stubMemorySearcher{}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stub.lastScope != "" {
		t.Errorf("expected empty scope on no-param request, got %q", stub.lastScope)
	}
}

// TestMemorySearchAction_ResultIncludesRepoScopeField pins the
// follow-on to the B-6 scope picker: every result must carry the
// chunk's own repo_scope so the template can render a "scope X"
// badge per row. Without this, the operator can't disambiguate
// "matched my scope filter" from "leaked through as
// uncategorized" from "cross-cutting *".
func TestMemorySearchAction_ResultIncludesRepoScopeField(t *testing.T) {
	stub := &stubMemorySearcher{
		results: []MemorySearchResult{
			{ChunkID: "a", ProjectID: "p", SourceName: "x.md", Score: 0.5, RepoScope: "github.com/x/y"},
			{ChunkID: "b", ProjectID: "p", SourceName: "y.md", Score: 0.4, RepoScope: "*"},
			{ChunkID: "c", ProjectID: "p", SourceName: "z.md", Score: 0.3, RepoScope: ""},
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Results []MemorySearchResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Results) != 3 {
		t.Fatalf("want 3 results, got %d", len(body.Results))
	}
	if body.Results[0].RepoScope != "github.com/x/y" {
		t.Errorf("scope-tagged hit lost RepoScope; got %q", body.Results[0].RepoScope)
	}
	if body.Results[1].RepoScope != "*" {
		t.Errorf("cross-cutting hit lost RepoScope; got %q", body.Results[1].RepoScope)
	}
	if body.Results[2].RepoScope != "" {
		t.Errorf("uncategorized hit should serialize empty RepoScope; got %q", body.Results[2].RepoScope)
	}
}

// TestMemorySearchAction_OverlongScopeIsCapped pins the same
// 512-char guard the q param has — a pathological scope string
// shouldn't make it into the SQL placeholder uncapped.
func TestMemorySearchAction_OverlongScopeIsCapped(t *testing.T) {
	stub := &stubMemorySearcher{}
	srv := NewServer(WithMemorySearcher(stub))
	long := strings.Repeat("x", 2000)
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello&repo_scope="+long, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := len(stub.lastScope); got > 512 {
		t.Errorf("expected scope capped at 512 chars, got %d", got)
	}
}

// B-15: every UI recall must stamp retrieval context with
// actor_kind="ui" so the memory_retrieval_audit row carries the
// surface label. Without this, operator manual searches through the
// /ui/memory scope picker landed in the audit with NULL actor_kind
// — indistinguishable from a stray internal call. Pinned here so a
// future refactor of MemorySearchAction can't quietly regress.
func TestMemorySearchAction_StampsActorKindUI(t *testing.T) {
	stub := &stubMemorySearcher{}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest("GET", "/memory/proj/search?q=hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stub.lastCtx == nil {
		t.Fatal("searcher not called")
	}
	rc := memory.RetrievalContextFromContext(stub.lastCtx)
	if rc.ActorKind != "ui" {
		t.Errorf("actor_kind: got %q, want ui", rc.ActorKind)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
