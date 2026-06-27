// Package ui: tests for TaskPostMortemGenerate. Stubs the
// PostMortemExplainer interface so the handler can run end-to-end
// without an LLM.
package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type stubPostMortemExplainerWithFn struct {
	generateFn func(ctx context.Context, taskID string, forceRefresh bool) (*PostMortemResult, error)
}

func (s *stubPostMortemExplainerWithFn) Generate(ctx context.Context, taskID string, forceRefresh bool) (*PostMortemResult, error) {
	if s.generateFn != nil {
		return s.generateFn(ctx, taskID, forceRefresh)
	}
	return nil, nil
}

func TestTaskPostMortemGenerate_MissingID(t *testing.T) {
	srv := NewServer(WithPostMortemExplainer(&stubPostMortemExplainerWithFn{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks//post-mortem", nil)
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskPostMortemGenerate_NoExplainerWired(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/post-mortem", nil)
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskPostMortemGenerate_TaskNotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(
		WithPostMortemExplainer(&stubPostMortemExplainerWithFn{}),
		WithTaskRepository(repo),
	)
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/post-mortem", nil)
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskPostMortemGenerate_GenerateError_RedirectsWithError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1"}, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(repo),
		WithPostMortemExplainer(&stubPostMortemExplainerWithFn{
			generateFn: func(_ context.Context, _ string, _ bool) (*PostMortemResult, error) {
				return nil, errors.New("rate limited")
			},
		}),
	)
	form := url.Values{"force_refresh": []string{"true"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/post-mortem",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "post_mortem_error=") {
		t.Errorf("expected post_mortem_error in redirect: %q", rec.Header().Get("Location"))
	}
}

func TestTaskPostMortemGenerate_Generated(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1"}, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(repo),
		WithPostMortemExplainer(&stubPostMortemExplainerWithFn{
			generateFn: func(_ context.Context, _ string, _ bool) (*PostMortemResult, error) {
				return &PostMortemResult{Cached: false}, nil
			},
		}),
	)
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/post-mortem", nil)
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "post_mortem=generated") {
		t.Errorf("expected post_mortem=generated in redirect: %q", loc)
	}
}

func TestTaskPostMortemGenerate_Cached(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1"}, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(repo),
		WithPostMortemExplainer(&stubPostMortemExplainerWithFn{
			generateFn: func(_ context.Context, _ string, _ bool) (*PostMortemResult, error) {
				return &PostMortemResult{Cached: true}, nil
			},
		}),
	)
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/post-mortem", nil)
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "post_mortem=cached") {
		t.Errorf("expected post_mortem=cached in redirect: %q", loc)
	}
}
