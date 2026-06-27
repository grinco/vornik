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

// closeOne / TaskBulkClose — Track C batch task actions, the bulk-Close delta
// (cancel + retry bulk already shipped). Close composes from uiCloseTask, so a
// COMPLETED/FAILED task closes and a RUNNING task is skipped (ValidateTransition
// gate), matching the continue-on-error contract of cancel/retry.

func TestCloseOne_ClosesEligible(t *testing.T) {
	transitioned := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, persistence.TaskStatusCompleted), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitioned = true
			if to != persistence.TaskStatusClosed {
				t.Fatalf("close must target CLOSED, got %s", to)
			}
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/close", nil)
	if !srv.closeOne(context.Background(), req, "t1") {
		t.Fatal("closeOne(COMPLETED) = false, want true")
	}
	if !transitioned {
		t.Error("close must run the → CLOSED transition")
	}
}

func TestCloseOne_SkipsIneligible(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, persistence.TaskStatusRunning), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Fatal("RUNNING must be rejected before TransitionConditional")
			return false, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/close", nil)
	if srv.closeOne(context.Background(), req, "t1") {
		t.Fatal("closeOne(RUNNING) = true, want false (skipped)")
	}
}

func TestTaskBulkClose_Multiple(t *testing.T) {
	statuses := map[string]persistence.TaskStatus{
		"t1": persistence.TaskStatusCompleted,
		"t2": persistence.TaskStatusRunning, // ineligible → skipped
		"t3": persistence.TaskStatusFailed,
	}
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return uiTask(id, statuses[id]), nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&uiTcStubMsgRepo{}))

	form := url.Values{"task_ids": []string{"t1", "t2", "t3"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/close", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkClose(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notice=bulk-closed") || !strings.Contains(loc, "count=2") {
		t.Errorf("redirect missing bulk-closed/count=2: %q", loc)
	}
}

func TestTaskBulkClose_NoIDs_RedirectsToList(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}), WithTaskMessageRepository(&uiTcStubMsgRepo{}))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks-bulk/close", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.TaskBulkClose(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/ui/tasks" {
		t.Fatalf("no-ids: got %d %q, want 303 /ui/tasks", rec.Code, rec.Header().Get("Location"))
	}
}

func TestTaskBulkClose_MethodNotAllowed(t *testing.T) {
	srv := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks-bulk/close", nil)
	rec := httptest.NewRecorder()
	srv.TaskBulkClose(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
}
