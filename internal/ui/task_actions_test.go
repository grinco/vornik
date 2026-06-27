// Package ui: hermetic tests for the task-action UI handlers
// (cancel / retry, single + bulk forms, partial status badge).
// All tests build a Server via NewServer + stub TaskRepository.
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

// uiTask returns a Task with the given status and sensible defaults.
func uiTask(id string, status persistence.TaskStatus) *persistence.Task {
	return &persistence.Task{
		ID:          id,
		ProjectID:   "p1",
		Status:      status,
		Attempt:     0,
		MaxAttempts: 3,
	}
}

// --- cancelOne -------------------------------------------------------

func TestCancelOne_NilOrError(t *testing.T) {
	cases := []struct {
		name string
		repo *mocks.MockTaskRepository
	}{
		{"task-not-found", &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return nil, nil
			},
		}},
		{"db-error", &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return nil, errors.New("db down")
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(WithTaskRepository(tc.repo))
			if got := srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != false {
				t.Errorf("expected false, got %v", got)
			}
		})
	}
}

func TestCancelOne_StatusGating(t *testing.T) {
	cases := []struct {
		name   string
		status persistence.TaskStatus
		want   bool
	}{
		{"queued", persistence.TaskStatusQueued, true},
		{"pending", persistence.TaskStatusPending, true},
		{"leased", persistence.TaskStatusLeased, true},
		{"running", persistence.TaskStatusRunning, true},
		// Non-terminal, non-executor-driven states are also cancellable
		// (state_machine.TriggerOperatorCancel allows any non-terminal).
		// Pre-fix WAITING_FOR_CHILDREN had no UI cancel path, leaving
		// parents stuck whenever the parent-unblock sweep didn't fire.
		{"waiting-for-children-cancellable", persistence.TaskStatusWaitingForChildren, true},
		{"awaiting-input-cancellable", persistence.TaskStatusAwaitingInput, true},
		{"awaiting-external-cancellable", persistence.TaskStatusAwaitingExternal, true},
		{"paused-cancellable", persistence.TaskStatusPaused, true},
		{"completed-not-cancellable", persistence.TaskStatusCompleted, false},
		{"failed-not-cancellable", persistence.TaskStatusFailed, false},
		{"closed-not-cancellable", persistence.TaskStatusClosed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updateCalled := false
			repo := &mocks.MockTaskRepository{
				GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
					return uiTask("t1", tc.status), nil
				},
				UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
					updateCalled = true
					return nil
				},
			}
			srv := NewServer(WithTaskRepository(repo))
			got := srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1")
			if got != tc.want {
				t.Errorf("status=%s: got %v, want %v", tc.status, got, tc.want)
			}
			if tc.want && !updateCalled {
				t.Errorf("status=%s: expected UpdateStatus to be called", tc.status)
			}
		})
	}
}

// --- TaskCancel ------------------------------------------------------

func TestTaskCancel_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.TaskCancel(rec, req, "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestTaskCancel_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.TaskCancel(rec, req, "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskCancel_NotCancellable_Redirect(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusCompleted), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.TaskCancel(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "task-not-cancellable") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
}

func TestTaskCancel_Success_Redirect(t *testing.T) {
	updates := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusQueued), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			updates++
			return nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/cancel", nil)
	rec := httptest.NewRecorder()
	srv.TaskCancel(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "task-cancelled") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
	if updates != 1 {
		t.Errorf("UpdateStatus calls: got %d, want 1", updates)
	}
}

// --- TaskBulkCancel --------------------------------------------------

func TestTaskBulkCancel_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks-bulk/cancel", nil)
	rec := httptest.NewRecorder()
	srv.TaskBulkCancel(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestTaskBulkCancel_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	form := url.Values{"task_ids": []string{"t1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkCancel(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskBulkCancel_NoIDs_RedirectsToList(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/cancel", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkCancel(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if rec.Header().Get("Location") != "/ui/tasks" {
		t.Errorf("location: got %q", rec.Header().Get("Location"))
	}
}

func TestTaskBulkCancel_Multiple(t *testing.T) {
	cancelled := 0
	statuses := map[string]persistence.TaskStatus{
		"t1": persistence.TaskStatusQueued,
		"t2": persistence.TaskStatusCompleted, // not cancellable
		"t3": persistence.TaskStatusRunning,
	}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, statuses[id]), nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			cancelled++
			return nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	form := url.Values{"task_ids": []string{"t1", "t2", "t3"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/cancel",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkCancel(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if cancelled != 2 {
		t.Errorf("expected 2 cancellations, got %d", cancelled)
	}
	if !strings.Contains(rec.Header().Get("Location"), "count=2") {
		t.Errorf("redirect location missing count=2: %q", rec.Header().Get("Location"))
	}
}

// --- TaskStatusPartial -----------------------------------------------

func TestTaskStatusPartial_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/status", nil)
	rec := httptest.NewRecorder()
	srv.TaskStatusPartial(rec, req, "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestTaskStatusPartial_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/status", nil)
	rec := httptest.NewRecorder()
	srv.TaskStatusPartial(rec, req, "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskStatusPartial_StopPollingOnTerminal(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusCompleted), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/status", nil)
	rec := httptest.NewRecorder()
	srv.TaskStatusPartial(rec, req, "t1")
	if got := rec.Header().Get("HX-Trigger"); got != "stopPolling" {
		t.Errorf("HX-Trigger: got %q, want stopPolling", got)
	}
}

func TestTaskStatusPartial_ContinuePollingWhileActive(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusRunning), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/status", nil)
	rec := httptest.NewRecorder()
	srv.TaskStatusPartial(rec, req, "t1")
	if got := rec.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("HX-Trigger: got %q, want empty (continue polling)", got)
	}
}

// --- retryOne --------------------------------------------------------

func TestRetryOne_NotFoundOrError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	if got := srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != false {
		t.Errorf("expected false on db error")
	}
}

func TestRetryOne_NonTerminal_Rejected(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusRunning), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	if got := srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != false {
		t.Errorf("expected false for RUNNING")
	}
}

func TestRetryOne_TerminalStates(t *testing.T) {
	for _, status := range []persistence.TaskStatus{
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusCompleted,
		persistence.TaskStatusPending,
	} {
		t.Run(string(status), func(t *testing.T) {
			repo := &mocks.MockTaskRepository{
				GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
					return uiTask("t1", status), nil
				},
				RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
					return true, nil
				},
			}
			srv := NewServer(WithTaskRepository(repo))
			if got := srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != true {
				t.Errorf("status=%s: expected true", status)
			}
		})
	}
}

func TestRetryOne_RequeueError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusFailed), nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			return false, errors.New("DB blew up")
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	if got := srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != false {
		t.Errorf("expected false on requeue error")
	}
}

func TestRetryOne_LostRaceWhenNotTransitioned(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusFailed), nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			return false, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	if got := srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1"); got != false {
		t.Errorf("lost race should return false")
	}
}

func TestRetryOne_MaxAttemptsExpansion(t *testing.T) {
	// When attempt > maxAttempts-1, maxAttempts grows so retry isn't
	// rejected by the requeue-time gate.
	var gotMaxAtts int
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			task := uiTask("t1", persistence.TaskStatusFailed)
			task.Attempt = 5
			task.MaxAttempts = 3
			return task, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, attempt, maxAtts int) (bool, error) {
			gotMaxAtts = maxAtts
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	srv.retryOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1")
	// attempt becomes 6, max becomes 8 (6+2)
	if gotMaxAtts != 8 {
		t.Errorf("max_attempts: got %d, want 8", gotMaxAtts)
	}
}

// --- TaskRetry / TaskBulkRetry --------------------------------------

func TestTaskRetry_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskRetry_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskRetry_NotRetriable(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusRunning), nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "task-not-retriable") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
}

func TestTaskRetry_Success(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("t1", persistence.TaskStatusFailed), nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "task-retried") {
		t.Errorf("location: %q", rec.Header().Get("Location"))
	}
}

func TestTaskBulkRetry_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks-bulk/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskBulkRetry(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskBulkRetry_RepoMissingReturns503(t *testing.T) {
	srv := NewServer()
	form := url.Values{"task_ids": []string{"t1"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/retry", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkRetry(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

func TestTaskBulkRetry_NoIDs(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/retry", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkRetry(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if rec.Header().Get("Location") != "/ui/tasks" {
		t.Errorf("location: got %q", rec.Header().Get("Location"))
	}
}

func TestTaskBulkRetry_Multiple(t *testing.T) {
	retried := 0
	statuses := map[string]persistence.TaskStatus{
		"t1": persistence.TaskStatusFailed,
		"t2": persistence.TaskStatusRunning, // not retriable
		"t3": persistence.TaskStatusCompleted,
	}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, statuses[id]), nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			retried++
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))
	form := url.Values{"task_ids": []string{"t1", "t2", "t3"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/retry",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkRetry(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d", rec.Code)
	}
	if retried != 2 {
		t.Errorf("expected 2 retries, got %d", retried)
	}
	if !strings.Contains(rec.Header().Get("Location"), "count=2") {
		t.Errorf("redirect location missing count=2: %q", rec.Header().Get("Location"))
	}
}
