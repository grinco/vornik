// Package api: tests for MemorySearch — the GET endpoint that
// surfaces hybrid-search hits for the operator UI / CLI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

type stubMemorySearcher struct {
	searchFn func(ctx context.Context, projectID, q string, limit int) ([]MemorySearchResult, error)
	gotLimit int
	gotCtx   context.Context
}

func (s *stubMemorySearcher) Search(ctx context.Context, projectID, q string, limit int) ([]MemorySearchResult, error) {
	s.gotLimit = limit
	s.gotCtx = ctx
	if s.searchFn != nil {
		return s.searchFn(ctx, projectID, q, limit)
	}
	return nil, nil
}

func TestMemorySearch_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/search", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemorySearch_MissingProjectID(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//memory/search?q=x", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemorySearch_MissingQuery(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Errorf("missing validation error: %s", rec.Body.String())
	}
}

func TestMemorySearch_NoSearcher(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemorySearch_SearcherError(t *testing.T) {
	stub := &stubMemorySearcher{
		searchFn: func(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
			return nil, errors.New("boom")
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestMemorySearch_HappyPath(t *testing.T) {
	stub := &stubMemorySearcher{
		searchFn: func(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
			return []MemorySearchResult{
				{ChunkID: "c1", Content: "hit-1"},
				{ChunkID: "c2", Content: "hit-2"},
			}, nil
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp memorySearchResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Results) != 2 {
		t.Errorf("results: got %d, want 2", len(resp.Results))
	}
}

// B-15: every REST recall must stamp retrieval context with
// actor_kind="rest_api" so the memory_retrieval_audit row carries
// the source surface. Pre-fix this row used to land with NULL
// actor_kind / NULL actor_id and muddied the dashboards that split
// companion vs operator-direct searches.
func TestMemorySearch_StampsActorKindRestAPI(t *testing.T) {
	stub := &stubMemorySearcher{
		searchFn: func(ctx context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	// Stamp an api key id on the request ctx the way AuthMiddleware
	// would: handlers read it via APIKeyIDFromContext to populate
	// actor_id on the audit row.
	ctx := context.WithValue(req.Context(), apiKeyIDKey, "akey_test_xyz")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if stub.gotCtx == nil {
		t.Fatal("Search not called")
	}
	rc := memory.RetrievalContextFromContext(stub.gotCtx)
	if rc.ActorKind != "rest_api" {
		t.Errorf("actor_kind: got %q, want %q", rc.ActorKind, "rest_api")
	}
	if rc.ActorID != "akey_test_xyz" {
		t.Errorf("actor_id: got %q, want %q", rc.ActorID, "akey_test_xyz")
	}
}

// Static-keys-only callers don't have an api_keys.id — stamping
// must still set actor_kind so the surface is identifiable, with
// an empty actor_id rather than dropping the stamp.
func TestMemorySearch_StampsActorKindRestAPI_NoKeyID(t *testing.T) {
	stub := &stubMemorySearcher{}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	rc := memory.RetrievalContextFromContext(stub.gotCtx)
	if rc.ActorKind != "rest_api" {
		t.Errorf("actor_kind: got %q, want rest_api", rc.ActorKind)
	}
	if rc.ActorID != "" {
		t.Errorf("actor_id: got %q, want empty", rc.ActorID)
	}
}

func TestMemorySearch_LimitClamping(t *testing.T) {
	stub := &stubMemorySearcher{
		searchFn: func(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	cases := []struct {
		query     string
		wantLimit int
	}{
		{"q=x", 10},
		{"q=x&limit=5", 5},
		{"q=x&limit=999", 50},
		{"q=x&limit=banana", 10},
		{"q=x&limit=-1", 10},
		{"q=x&limit=0", 10},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			stub.gotLimit = 0
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/p1/memory/search?"+tc.query, nil)
			rec := httptest.NewRecorder()
			srv.MemorySearch(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d", rec.Code)
			}
			if stub.gotLimit != tc.wantLimit {
				t.Errorf("limit: got %d, want %d", stub.gotLimit, tc.wantLimit)
			}
		})
	}
}
