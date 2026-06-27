package ui

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Autonomy manual-approval surface — UI action helpers
// (https://docs.vornik.io). The
// operator approves (→ QUEUED) or rejects (→ CANCELLED) a task parked
// in AWAITING_APPROVAL from the task detail page.

func TestUIApproveTask_HappyPath(t *testing.T) {
	var gotFrom []persistence.TaskStatus
	var gotTo persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			gotFrom, gotTo = from, to
			if !opts.ClearLease {
				t.Error("approve must clear the lease so the scheduler can lease the task")
			}
			return true, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingApproval}
	if got := srv.uiApproveTask(context.Background(), task); got != "approved" {
		t.Fatalf("uiApproveTask = %q, want approved", got)
	}
	if gotTo != persistence.TaskStatusQueued {
		t.Errorf("approve target = %q, want QUEUED", gotTo)
	}
	if len(gotFrom) != 1 || gotFrom[0] != persistence.TaskStatusAwaitingApproval {
		t.Errorf("approve from-set = %v, want [AWAITING_APPROVAL]", gotFrom)
	}
}

func TestUIRejectTask_HappyPath(t *testing.T) {
	var gotTo persistence.TaskStatus
	taskRepo := &mocks.MockTaskRepository{
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			gotTo = to
			return true, nil
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
	task := &persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingApproval}
	if got := srv.uiRejectTask(context.Background(), task); got != "rejected" {
		t.Fatalf("uiRejectTask = %q, want rejected", got)
	}
	if gotTo != persistence.TaskStatusCancelled {
		t.Errorf("reject target = %q, want CANCELLED", gotTo)
	}
}
