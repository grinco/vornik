// Regression tests for the 2026-06-07 child-cancel incident: the UI
// cancel path (single + bulk, via cancelOne) flipped the child to
// CANCELLED but never drove the executor's parent-unblock sweep, so
// a parent in WAITING_FOR_CHILDREN waited for the cancelled child
// forever. Mirrors the close path's NotifyChildTerminal wiring
// (task_conversation_actions_test.go).
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestCancelOne_NotifiesExecutorOnChildCancel(t *testing.T) {
	parentID := "parent-1"
	child := uiTask("child-1", persistence.TaskStatusQueued)
	child.ParentTaskID = &parentID
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return child, nil
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(repo), WithExecutor(spy))

	if !srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "child-1") {
		t.Fatal("cancelOne = false, want true")
	}
	if len(spy.calls) != 1 || spy.calls[0] != "child-1" {
		t.Errorf("NotifyChildTerminal calls = %v, want [child-1]", spy.calls)
	}
}

// Cancelling a root task must not fire the notification — nothing to
// unblock. Mirrors TestUICloseTask_NoNotifyForRootTask.
func TestCancelOne_NoNotifyForRootTask(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return uiTask("root-1", persistence.TaskStatusQueued), nil
		},
	}
	spy := &closeNotifySpy{}
	srv := NewServer(WithTaskRepository(repo), WithExecutor(spy))

	if !srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "root-1") {
		t.Fatal("cancelOne = false, want true")
	}
	if len(spy.calls) != 0 {
		t.Errorf("NotifyChildTerminal calls = %v, want none for root task", spy.calls)
	}
}
