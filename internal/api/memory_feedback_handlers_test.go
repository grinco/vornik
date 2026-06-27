// Package api: hermetic tests for the memory-feedback chunk-utility
// surface. Wires a stub MemoryRetrievalAuditRepository into a Server
// and drives the handler via httptest.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type stubMemoryAuditRepo struct {
	persistence.MemoryRetrievalAuditRepository

	feedbackFn     func(ctx context.Context, projectID string, since time.Time) (*persistence.MemoryFeedbackStats, error)
	unretrievedFn  func(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error)
	gotProjectID   string
	gotSince       time.Time
	gotSampleLimit int
}

func (s *stubMemoryAuditRepo) FeedbackStats(ctx context.Context, projectID string, since time.Time) (*persistence.MemoryFeedbackStats, error) {
	s.gotProjectID = projectID
	s.gotSince = since
	if s.feedbackFn != nil {
		return s.feedbackFn(ctx, projectID, since)
	}
	return &persistence.MemoryFeedbackStats{}, nil
}

func (s *stubMemoryAuditRepo) UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error) {
	s.gotSampleLimit = limit
	if s.unretrievedFn != nil {
		return s.unretrievedFn(ctx, projectID, since, limit)
	}
	return nil, nil
}

func TestMemoryFeedback_MethodNotAllowed(t *testing.T) {
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestMemoryFeedback_NotConfigured(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestMemoryFeedback_MissingProjectID(t *testing.T) {
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestMemoryFeedback_FeedbackStatsError(t *testing.T) {
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{
		feedbackFn: func(_ context.Context, _ string, _ time.Time) (*persistence.MemoryFeedbackStats, error) {
			return nil, errors.New("query failed")
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestMemoryFeedback_QueryParamClamping(t *testing.T) {
	repo := &stubMemoryAuditRepo{
		feedbackFn: func(_ context.Context, _ string, _ time.Time) (*persistence.MemoryFeedbackStats, error) {
			return &persistence.MemoryFeedbackStats{TotalChunks: 10, UnretrievedChunks: 3}, nil
		},
		unretrievedFn: func(_ context.Context, _ string, _ time.Time, limit int) ([]string, error) {
			return []string{"c1", "c2"}, nil
		},
	}
	cases := []struct {
		name           string
		url            string
		wantSampleSize int
	}{
		// days/sample are validated only on their max bounds; the
		// service ceiling is days=365, sample=200.
		{"default", "/api/v1/projects/p1/memory/feedback", 20},
		{"days-and-sample", "/api/v1/projects/p1/memory/feedback?days=7&sample=5", 5},
		{"days-too-high-clamps-to-365", "/api/v1/projects/p1/memory/feedback?days=9999", 20},
		{"sample-too-high-clamps-to-200", "/api/v1/projects/p1/memory/feedback?sample=9999", 200},
		{"days-negative-uses-default", "/api/v1/projects/p1/memory/feedback?days=-5", 20},
		{"sample-zero-skips-fetch", "/api/v1/projects/p1/memory/feedback?sample=0", 0},
		{"non-numeric-days-uses-default", "/api/v1/projects/p1/memory/feedback?days=abc", 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo.gotSampleLimit = 0
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rec := httptest.NewRecorder()
			srv := &Server{memoryAuditRepo: repo}
			srv.MemoryFeedback(rec, req, "p1")
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
			}
			if tc.wantSampleSize > 0 && repo.gotSampleLimit != tc.wantSampleSize {
				t.Errorf("sample limit: got %d, want %d", repo.gotSampleLimit, tc.wantSampleSize)
			}
		})
	}
}

func TestMemoryFeedback_HappyPath_WithSampleIDs(t *testing.T) {
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{
		feedbackFn: func(_ context.Context, _ string, _ time.Time) (*persistence.MemoryFeedbackStats, error) {
			return &persistence.MemoryFeedbackStats{
				TotalChunks:       100,
				RetrievedChunks:   75,
				UnretrievedChunks: 25,
				TotalSearches:     500,
			}, nil
		},
		unretrievedFn: func(_ context.Context, _ string, _ time.Time, _ int) ([]string, error) {
			return []string{"chunk-a", "chunk-b"}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/feedback?days=14&sample=10", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp memoryFeedbackResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.WindowDays != 14 {
		t.Errorf("WindowDays: got %d, want 14", resp.WindowDays)
	}
	if resp.TotalChunks != 100 || resp.RetrievedChunks != 75 {
		t.Errorf("counts: %+v", resp)
	}
	if len(resp.UnretrievedSampleIDs) != 2 {
		t.Errorf("expected 2 sample IDs, got %v", resp.UnretrievedSampleIDs)
	}
}

func TestMemoryFeedback_HappyPath_NoUnretrievedSkipsSecondQuery(t *testing.T) {
	called := false
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{
		feedbackFn: func(_ context.Context, _ string, _ time.Time) (*persistence.MemoryFeedbackStats, error) {
			return &persistence.MemoryFeedbackStats{
				TotalChunks:     10,
				RetrievedChunks: 10,
				// UnretrievedChunks=0 → skip the IDs query
			}, nil
		},
		unretrievedFn: func(_ context.Context, _ string, _ time.Time, _ int) ([]string, error) {
			called = true
			return nil, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if called {
		t.Errorf("UnretrievedChunkIDs should not be queried when UnretrievedChunks=0")
	}
}

func TestMemoryFeedback_UnretrievedErrorIsSwallowed(t *testing.T) {
	srv := &Server{memoryAuditRepo: &stubMemoryAuditRepo{
		feedbackFn: func(_ context.Context, _ string, _ time.Time) (*persistence.MemoryFeedbackStats, error) {
			return &persistence.MemoryFeedbackStats{UnretrievedChunks: 5, TotalChunks: 10}, nil
		},
		unretrievedFn: func(_ context.Context, _ string, _ time.Time, _ int) ([]string, error) {
			return nil, errors.New("query failed")
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/feedback", nil)
	rec := httptest.NewRecorder()
	srv.MemoryFeedback(rec, req, "p1")
	// Response is still 200 — the secondary query is best-effort.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"unretrieved_sample_ids"`) {
		t.Errorf("expected sample_ids omitted on error, got: %s", body)
	}
}
