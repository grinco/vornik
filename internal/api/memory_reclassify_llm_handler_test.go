package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// stubClassifyBackfiller is a deterministic MemoryClassifyBackfiller
// for the handler tests. Captures arguments so assertions can check
// the handler forwarded the right project + batch size.
type stubClassifyBackfiller struct {
	remaining    int
	remainingErr error
	batchResult  *MemoryClassifyBackfillResult
	batchErr     error
	gotProjectID string
	gotBatchSize int
	countCalls   int
	batchCalls   int
}

func (s *stubClassifyBackfiller) CountRemaining(_ context.Context, projectID string) (int, error) {
	s.countCalls++
	s.gotProjectID = projectID
	if s.remainingErr != nil {
		return 0, s.remainingErr
	}
	return s.remaining, nil
}

func (s *stubClassifyBackfiller) BackfillBatch(_ context.Context, projectID string, batchSize int) (*MemoryClassifyBackfillResult, error) {
	s.batchCalls++
	s.gotProjectID = projectID
	s.gotBatchSize = batchSize
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	return s.batchResult, nil
}

func newReclassifyServer(b MemoryClassifyBackfiller) *Server {
	return &Server{
		logger:                   zerolog.Nop(),
		memoryClassifyBackfiller: b,
	}
}

func TestMemoryReclassifyLLM_RejectsNonPost(t *testing.T) {
	s := newReclassifyServer(&stubClassifyBackfiller{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/memory/reclassify-llm?project=p", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestMemoryReclassifyLLM_DisabledWhenBackfillerNil(t *testing.T) {
	s := newReclassifyServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MEMORY_CLASSIFIER_DISABLED") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestMemoryReclassifyLLM_RequiresProjectQuery(t *testing.T) {
	s := newReclassifyServer(&stubClassifyBackfiller{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestMemoryReclassifyLLM_RejectsForeignScopedProject(t *testing.T) {
	stub := &stubClassifyBackfiller{remaining: 42}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=project-b&count=true", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, projectIDKey, []string{"project-a"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if stub.countCalls != 0 || stub.batchCalls != 0 {
		t.Fatalf("backfiller should not be called for foreign project")
	}
}

func TestMemoryReclassifyLLM_CountProbe(t *testing.T) {
	stub := &stubClassifyBackfiller{remaining: 42}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=assistant&count=true", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	if stub.countCalls != 1 {
		t.Fatalf("countCalls: %d", stub.countCalls)
	}
	if stub.batchCalls != 0 {
		t.Fatalf("batchCalls should be 0 on count probe, got %d", stub.batchCalls)
	}
	if stub.gotProjectID != "assistant" {
		t.Fatalf("projectID forwarded wrong: %q", stub.gotProjectID)
	}
	var out MemoryClassifyBackfillResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Remaining != 42 {
		t.Fatalf("remaining: %d", out.Remaining)
	}
}

func TestMemoryReclassifyLLM_CountErrorReturns500(t *testing.T) {
	stub := &stubClassifyBackfiller{remainingErr: errors.New("db down")}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p&count=true", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestMemoryReclassifyLLM_BatchHappyPath(t *testing.T) {
	stub := &stubClassifyBackfiller{
		batchResult: &MemoryClassifyBackfillResult{
			Processed: 10, Succeeded: 9, Skipped: 1, Remaining: 5,
		},
	}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p&batch_size=10", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	if stub.batchCalls != 1 || stub.gotBatchSize != 10 {
		t.Fatalf("batch wiring: calls=%d size=%d", stub.batchCalls, stub.gotBatchSize)
	}
	var out MemoryClassifyBackfillResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Processed != 10 || out.Succeeded != 9 || out.Skipped != 1 {
		t.Fatalf("decoded: %+v", out)
	}
}

func TestMemoryReclassifyLLM_BatchSizeClampedAt50(t *testing.T) {
	stub := &stubClassifyBackfiller{batchResult: &MemoryClassifyBackfillResult{}}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p&batch_size=999", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if stub.gotBatchSize != 50 {
		t.Fatalf("batch_size not clamped to 50: %d", stub.gotBatchSize)
	}
}

func TestMemoryReclassifyLLM_BatchSizeInvalidFallsBackToDefault(t *testing.T) {
	stub := &stubClassifyBackfiller{batchResult: &MemoryClassifyBackfillResult{}}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p&batch_size=not-a-number", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if stub.gotBatchSize != 10 {
		t.Fatalf("invalid batch_size should fall back to default 10: %d", stub.gotBatchSize)
	}
}

func TestMemoryReclassifyLLM_BatchErrorReturns500(t *testing.T) {
	stub := &stubClassifyBackfiller{batchErr: errors.New("classifier model down")}
	s := newReclassifyServer(stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/reclassify-llm?project=p", nil)
	rec := httptest.NewRecorder()
	s.MemoryReclassifyLLM(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", rec.Code)
	}
}
