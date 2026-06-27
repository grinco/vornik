package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func execCancelRig(statuses map[string]persistence.ExecutionStatus) (*mocks.MockExecutionRepository, *mocks.MockTaskRepository, *int) {
	cancelled := 0
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			st, ok := statuses[id]
			if !ok {
				return nil, nil
			}
			return &persistence.Execution{ID: id, TaskID: "task-" + id, Status: st}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.ExecutionStatus) error {
			cancelled++
			return nil
		},
	}
	taskRepo := &mocks.MockTaskRepository{
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error { return nil },
	}
	return execRepo, taskRepo, &cancelled
}

func TestCancelExecutionOne_Cancels(t *testing.T) {
	execRepo, taskRepo, cancelled := execCancelRig(map[string]persistence.ExecutionStatus{
		"e1": persistence.ExecutionStatusPending,
	})
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/ui/executions-bulk/cancel", nil)
	if !srv.cancelExecutionOne(context.Background(), req, "e1") {
		t.Fatal("cancelExecutionOne(found) = false, want true")
	}
	if *cancelled == 0 {
		t.Error("execution status was not updated")
	}
}

func TestCancelExecutionOne_NotFound(t *testing.T) {
	execRepo, taskRepo, _ := execCancelRig(map[string]persistence.ExecutionStatus{})
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(taskRepo))

	req := httptest.NewRequest(http.MethodPost, "/ui/executions-bulk/cancel", nil)
	if srv.cancelExecutionOne(context.Background(), req, "missing") {
		t.Fatal("cancelExecutionOne(missing) = true, want false")
	}
}

func TestExecutionBulkCancel_Multiple(t *testing.T) {
	execRepo, taskRepo, _ := execCancelRig(map[string]persistence.ExecutionStatus{
		"e1": persistence.ExecutionStatusPending,
		"e2": persistence.ExecutionStatusRunning,
		"e3": persistence.ExecutionStatusPending,
	})
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(taskRepo))

	form := url.Values{"exec_ids": []string{"e1", "e2", "e3"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/executions-bulk/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ExecutionBulkCancel(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=bulk-exec-cancelled") || !strings.Contains(loc, "count=3") {
		t.Errorf("redirect missing bulk-exec-cancelled/count=3: %q", loc)
	}
}

func TestExecutionBulkCancel_NoIDs_RedirectsToList(t *testing.T) {
	srv := NewServer(WithExecutionRepository(&mocks.MockExecutionRepository{}), WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/executions-bulk/cancel", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ExecutionBulkCancel(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/executions" {
		t.Fatalf("no-ids: got %d %q, want 303 /ui/executions", rec.Code, rec.Header().Get("Location"))
	}
}

func TestExecutionBulkCancel_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithExecutionRepository(&mocks.MockExecutionRepository{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/executions-bulk/cancel", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionBulkCancel(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}
