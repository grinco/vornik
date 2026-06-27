package chat

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestExecuteAction_DestructiveConfirmationGate is the hardening
// regression (2026-06-15, security LLD review batch 3): cancel_task and
// retry_task MUST NOT execute without Confirm=true — they return a
// confirmation prompt instead, and the task repo is never mutated.
func TestExecuteAction_DestructiveConfirmationGate(t *testing.T) {
	ctx := context.Background()
	mutated := false
	tr := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error {
			mutated = true
			return nil
		},
		UpdateFunc: func(_ context.Context, _ *persistence.Task) error {
			mutated = true
			return nil
		},
	}
	er := &mocks.MockExecutionRepository{}

	for _, at := range []string{ActionCancelTask, ActionRetryTask} {
		mutated = false
		res, err := ExecuteAction(ctx, Action{Type: at, TaskID: "t1"}, tr, er, nil)
		if err != nil {
			t.Fatalf("%s unconfirmed: unexpected err %v", at, err)
		}
		if res.Success {
			t.Errorf("%s executed without confirmation", at)
		}
		if !res.NeedsConfirmation {
			t.Errorf("%s: NeedsConfirmation = false, want true (%+v)", at, res)
		}
		if mutated {
			t.Errorf("%s mutated the task repo without confirmation", at)
		}
	}
}

// TestExecuteAction_Routing walks every action type via ExecuteAction
// to make sure the dispatch switch lands on the right helper, plus the
// "Unknown action" default branch.
func TestExecuteAction_Routing(t *testing.T) {
	ctx := context.Background()
	tr := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
		CreateFunc: func(_ context.Context, _ *persistence.Task) error { return nil },
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, _ persistence.TaskStatus) error { return nil },
		UpdateFunc:       func(_ context.Context, _ *persistence.Task) error { return nil },
	}
	er := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return nil, nil
		},
	}

	// list_tasks happy.
	res, err := ExecuteAction(ctx, Action{Type: ActionListTasks, Project: "p", Status: "queued"}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("list_tasks failed: %v / %+v", err, res)
	}

	// list_tasks with rows.
	tr.ListFunc = func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
		return []*persistence.Task{{ID: "t1", ProjectID: "p", Status: persistence.TaskStatusQueued}}, nil
	}
	res, err = ExecuteAction(ctx, Action{Type: ActionListTasks}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("list_tasks rows: %v / %+v", err, res)
	}

	// create_task happy.
	res, err = ExecuteAction(ctx, Action{Type: ActionCreateTask, Project: "p", Type_: "x", Input: map[string]any{"k": "v"}, WorkflowID: "wf"}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("create_task: %v / %+v", err, res)
	}
	// create_task guard branches.
	res, _ = ExecuteAction(ctx, Action{Type: ActionCreateTask}, tr, er, nil)
	if res.Success {
		t.Error("create_task without project should fail")
	}
	res, _ = ExecuteAction(ctx, Action{Type: ActionCreateTask, Project: "p"}, tr, er, nil)
	if res.Success {
		t.Error("create_task without type should fail")
	}

	// Destructive actions require Confirm=true; the execution-logic cases
	// below pass it (the gate itself is covered by TestExecuteAction_
	// DestructiveConfirmationGate).
	// cancel_task happy.
	res, err = ExecuteAction(ctx, Action{Type: ActionCancelTask, TaskID: "t1", Confirm: true}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("cancel_task: %v / %+v", err, res)
	}
	// cancel_task already terminal.
	tr.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
	}
	res, _ = ExecuteAction(ctx, Action{Type: ActionCancelTask, TaskID: "t1", Confirm: true}, tr, er, nil)
	if res.Success {
		t.Error("cancel_task on terminal should fail")
	}
	// cancel_task missing id.
	res, _ = ExecuteAction(ctx, Action{Type: ActionCancelTask, Confirm: true}, tr, er, nil)
	if res.Success {
		t.Error("cancel_task without id should fail")
	}

	// retry_task happy (set Status=FAILED).
	tr.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusFailed}, nil
	}
	res, err = ExecuteAction(ctx, Action{Type: ActionRetryTask, TaskID: "t1", Confirm: true}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("retry_task: %v / %+v", err, res)
	}
	// retry_task non-failed.
	tr.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	res, _ = ExecuteAction(ctx, Action{Type: ActionRetryTask, TaskID: "t1", Confirm: true}, tr, er, nil)
	if res.Success {
		t.Error("retry_task on RUNNING should fail")
	}
	// retry_task missing id.
	res, _ = ExecuteAction(ctx, Action{Type: ActionRetryTask, Confirm: true}, tr, er, nil)
	if res.Success {
		t.Error("retry_task without id should fail")
	}

	// get_status happy.
	tr.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		errMsg := "boom"
		return &persistence.Task{
			ID:        id,
			Status:    persistence.TaskStatusFailed,
			Priority:  50,
			Attempt:   1,
			LastError: &errMsg,
			Payload:   []byte(`{"context":{"prompt":"do the thing"}}`),
		}, nil
	}
	er.GetByTaskIDFunc = func(_ context.Context, _ string) (*persistence.Execution, error) {
		errMsg := "exec boom"
		return &persistence.Execution{
			ID:           "e1",
			Status:       persistence.ExecutionStatusFailed,
			ErrorMessage: &errMsg,
			Result:       []byte(`{"message":"finished"}`),
		}, nil
	}
	res, err = ExecuteAction(ctx, Action{Type: ActionGetStatus, TaskID: "t1"}, tr, er, nil)
	if err != nil || !res.Success {
		t.Errorf("get_status: %v / %+v", err, res)
	}
	// get_status missing id.
	res, _ = ExecuteAction(ctx, Action{Type: ActionGetStatus}, tr, er, nil)
	if res.Success {
		t.Error("get_status without id should fail")
	}

	// Unknown action.
	res, err = ExecuteAction(ctx, Action{Type: "bogus"}, tr, er, nil)
	if err == nil || res.Success {
		t.Errorf("unknown action should fail, got %+v", res)
	}
}

// TestExecuteAction_RepoErrors propagates repository errors.
func TestExecuteAction_RepoErrors(t *testing.T) {
	ctx := context.Background()
	tr := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("list boom")
		},
		CreateFunc: func(_ context.Context, _ *persistence.Task) error { return errors.New("create boom") },
		GetFunc:    func(_ context.Context, _ string) (*persistence.Task, error) { return nil, errors.New("get boom") },
	}
	er := &mocks.MockExecutionRepository{}

	if _, err := ExecuteAction(ctx, Action{Type: ActionListTasks}, tr, er, nil); err == nil {
		t.Error("list error should propagate")
	}
	if _, err := ExecuteAction(ctx, Action{Type: ActionCreateTask, Project: "p", Type_: "x"}, tr, er, nil); err == nil {
		t.Error("create error should propagate")
	}
	if _, err := ExecuteAction(ctx, Action{Type: ActionCancelTask, TaskID: "t1", Confirm: true}, tr, er, nil); err == nil {
		t.Error("cancel get error should propagate")
	}
	if _, err := ExecuteAction(ctx, Action{Type: ActionRetryTask, TaskID: "t1", Confirm: true}, tr, er, nil); err == nil {
		t.Error("retry get error should propagate")
	}
	if _, err := ExecuteAction(ctx, Action{Type: ActionGetStatus, TaskID: "t1"}, tr, er, nil); err == nil {
		t.Error("get_status get error should propagate")
	}
}
