package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Autonomy manual-approval surface
// (https://docs.vornik.io). Tasks
// created under a project with requireApproval land in
// AWAITING_APPROVAL; the operator resolves them via approve (→ QUEUED,
// the scheduler then leases normally) or reject (→ CANCELLED, never
// runs). Before this surface existed they were parked in PENDING and
// waited forever (operator report 2026-06-09).

func TestApproveTask_RequeuesAwaitingApproval(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-awaiting-approval-1"

	transitionCalled := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusAwaitingApproval,
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			transitionCalled = true
			if to != persistence.TaskStatusQueued {
				t.Fatalf("approve should target QUEUED, got %s", to)
			}
			if len(from) != 1 || from[0] != persistence.TaskStatusAwaitingApproval {
				t.Fatalf("approve must gate on AWAITING_APPROVAL, got %v", from)
			}
			if !opts.ClearLease {
				t.Error("approve must set ClearLease=true so scheduler can lease the task")
			}
			return true, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	s := buildPauseResumeServer(taskRepo, msgRepo, &mockPauseResumeExecutor{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/approve", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.ApproveTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !transitionCalled {
		t.Error("approve must run the AWAITING_APPROVAL → QUEUED transition")
	}
	if len(msgRepo.inserted) != 1 {
		t.Errorf("approve must record an operator audit message, got %d", len(msgRepo.inserted))
	}
}

func TestRejectTask_CancelsAwaitingApproval(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-awaiting-approval-2"

	transitionCalled := false
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusAwaitingApproval,
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			transitionCalled = true
			if to != persistence.TaskStatusCancelled {
				t.Fatalf("reject should target CANCELLED, got %s", to)
			}
			if len(from) != 1 || from[0] != persistence.TaskStatusAwaitingApproval {
				t.Fatalf("reject must gate on AWAITING_APPROVAL, got %v", from)
			}
			return true, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	s := buildPauseResumeServer(taskRepo, msgRepo, &mockPauseResumeExecutor{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/reject", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.RejectTask(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !transitionCalled {
		t.Error("reject must run the AWAITING_APPROVAL → CANCELLED transition")
	}
}

// A stuck PENDING task (the overloaded at-rest status) has no pending
// approval and must NOT be approvable — the state machine rejects the
// transition, so the handler returns 409 without touching the DB.
func TestApproveTask_RejectsNonAwaitingApproval(t *testing.T) {
	const projectID = "test-proj"
	const taskID = "task-pending-stuck"

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:        id,
				ProjectID: projectID,
				Status:    persistence.TaskStatusPending,
			}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			t.Fatal("TransitionConditional must not run when the transition is invalid")
			return false, nil
		},
	}
	msgRepo := &stubTaskMessageRepo{}
	s := buildPauseResumeServer(taskRepo, msgRepo, &mockPauseResumeExecutor{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/tasks/"+taskID+"/approve", nil)
	req = withProjectAndTaskRouteVars(req, projectID, taskID)
	rec := httptest.NewRecorder()
	s.ApproveTask(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("approving a PENDING (non-AWAITING_APPROVAL) task must be 409, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}
