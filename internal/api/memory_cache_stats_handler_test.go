package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// stubCacheStatsProvider mirrors the production adapter shape so
// the handler can be exercised in isolation. Each field controls
// one of the two stats blocks the handler emits.
type stubCacheStatsProvider struct {
	embed    EmbeddingCacheStatsResult
	embedErr error
	resp     ResponseCacheStatsResult
	respErr  error
}

func (s *stubCacheStatsProvider) EmbeddingCacheStats(_ context.Context) (EmbeddingCacheStatsResult, error) {
	return s.embed, s.embedErr
}

func (s *stubCacheStatsProvider) ResponseCacheStats(_ context.Context) (ResponseCacheStatsResult, error) {
	return s.resp, s.respErr
}

func newCacheStatsServer(p MemoryCacheStatsProvider) *Server {
	return &Server{
		logger:           zerolog.Nop(),
		memoryCacheStats: p,
	}
}

func TestMemoryCacheStats_RejectsNonGet(t *testing.T) {
	s := newCacheStatsServer(&stubCacheStatsProvider{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/cache-stats", nil)
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestMemoryCacheStats_503WhenUnwired(t *testing.T) {
	s := newCacheStatsServer(nil)
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/memory/cache-stats", nil))
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestMemoryCacheStats_PopulatedHappyPath(t *testing.T) {
	p := &stubCacheStatsProvider{
		embed: EmbeddingCacheStatsResult{
			Enabled: true, RowCount: 42, ApproxBytes: 8192, DistinctModels: 1,
		},
		resp: ResponseCacheStatsResult{
			Enabled: true, RowCount: 17, ApproxBytes: 4096,
			DistinctPurposes: 3, TotalHits: 200, TotalSavingsUSD: 12.34,
		},
	}
	s := newCacheStatsServer(p)
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/memory/cache-stats", nil))
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		EmbeddingCache EmbeddingCacheStatsResult `json:"embedding_cache"`
		ResponseCache  ResponseCacheStatsResult  `json:"response_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EmbeddingCache.RowCount != 42 {
		t.Errorf("embedding rows: got %d, want 42", got.EmbeddingCache.RowCount)
	}
	if got.ResponseCache.TotalSavingsUSD != 12.34 {
		t.Errorf("savings: got %.2f, want 12.34", got.ResponseCache.TotalSavingsUSD)
	}
	if got.ResponseCache.TotalHits != 200 {
		t.Errorf("hits: got %d, want 200", got.ResponseCache.TotalHits)
	}
}

func TestMemoryCacheStats_BothErrorsReturns500(t *testing.T) {
	p := &stubCacheStatsProvider{
		embedErr: errors.New("db1 down"),
		respErr:  errors.New("db2 down"),
	}
	s := newCacheStatsServer(p)
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/memory/cache-stats", nil))
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestMemoryCacheStats_SingleErrorReturnsPartial(t *testing.T) {
	p := &stubCacheStatsProvider{
		embedErr: errors.New("table absent"),
		resp: ResponseCacheStatsResult{
			Enabled: true, RowCount: 1, TotalHits: 5, TotalSavingsUSD: 0.50,
		},
	}
	s := newCacheStatsServer(p)
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/memory/cache-stats", nil))
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with partial data, got %d", rec.Code)
	}
	var got struct {
		ResponseCache ResponseCacheStatsResult `json:"response_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ResponseCache.TotalSavingsUSD != 0.50 {
		t.Errorf("expected response cache half to survive, got %+v", got.ResponseCache)
	}
}

func TestMemoryCacheStats_DisabledRendersZeros(t *testing.T) {
	p := &stubCacheStatsProvider{
		embed: EmbeddingCacheStatsResult{Enabled: false},
		resp:  ResponseCacheStatsResult{Enabled: false},
	}
	s := newCacheStatsServer(p)
	req := authDisabledReq(httptest.NewRequest(http.MethodGet, "/api/v1/memory/cache-stats", nil))
	rec := httptest.NewRecorder()
	s.MemoryCacheStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got struct {
		EmbeddingCache EmbeddingCacheStatsResult `json:"embedding_cache"`
		ResponseCache  ResponseCacheStatsResult  `json:"response_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EmbeddingCache.Enabled || got.ResponseCache.Enabled {
		t.Error("disabled providers must return Enabled=false")
	}
}
