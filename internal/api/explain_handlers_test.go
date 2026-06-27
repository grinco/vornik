// Package api: hermetic tests for the explain endpoint. Uses a real
// postmortem.Renderer wired to a stub TaskRepository so the handler's
// dispatch + error mapping branches run end-to-end without a DB.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
)

func TestExplainTask_NotConfigured(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "EXPLAIN_NOT_CONFIGURED") {
		t.Errorf("missing error code: %s", rec.Body.String())
	}
}

func TestExplainTask_MethodNotAllowed(t *testing.T) {
	srv := &Server{explainRenderer: &postmortem.Renderer{}}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestExplainTask_ValidationMissingIDs(t *testing.T) {
	srv := &Server{explainRenderer: &postmortem.Renderer{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects//tasks//explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestExplainTask_ProjectMismatch_Returns404(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "OTHER", Status: persistence.TaskStatusFailed}, nil
		},
	}
	renderer := postmortem.NewRenderer(taskRepo, nil, nil, nil, nil)
	srv := &Server{taskRepo: taskRepo, explainRenderer: renderer}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestExplainTask_NotFoundFromRenderer(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, persistence.ErrNotFound
		},
	}
	renderer := postmortem.NewRenderer(taskRepo, nil, nil, nil, nil)
	srv := &Server{explainRenderer: renderer}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestExplainTask_RendererInternalError(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("db blew up")
		},
	}
	renderer := postmortem.NewRenderer(taskRepo, nil, nil, nil, nil)
	srv := &Server{explainRenderer: renderer}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "EXPLAIN_FAILED") {
		t.Errorf("missing error code: %s", rec.Body.String())
	}
}

func TestExplainTask_HappyPath(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed}, nil
		},
	}
	renderer := postmortem.NewRenderer(taskRepo, nil, nil, nil, nil)
	srv := &Server{taskRepo: taskRepo, explainRenderer: renderer}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p1/tasks/t1/explain", nil)
	rec := httptest.NewRecorder()
	srv.ExplainTask(rec, req, "p1", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp explainResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Inputs["task_id"] != "t1" {
		t.Errorf("expected task_id=t1 in inputs, got %v", resp.Inputs)
	}
}
