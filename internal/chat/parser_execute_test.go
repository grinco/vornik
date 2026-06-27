// Coverage for the parser package's executeXxx action handlers.
// ExecuteAction's switch was already covered (unknown-action path)
// but each branch beneath it was dark because no scripted task repo
// was wired. Each test here drives one action through the mock
// TaskRepository and asserts the result shape — success branches,
// error branches, and the "missing field" guards.

package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// ---- list_tasks --------------------------------------------------------

func TestExecuteAction_ListTasks_FiltersAndSummarises(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			// Project + status both flow through.
			if filter.ProjectID == nil || *filter.ProjectID != "p1" {
				t.Errorf("filter.ProjectID: got %v, want p1", filter.ProjectID)
			}
			if filter.Status == nil || *filter.Status != persistence.TaskStatusQueued {
				t.Errorf("filter.Status: got %v, want QUEUED", filter.Status)
			}
			return []*persistence.Task{
				{ID: "t-1", ProjectID: "p1", Status: persistence.TaskStatusQueued},
				{ID: "t-2", ProjectID: "p1", Status: persistence.TaskStatusQueued},
			}, nil
		},
	}
	action := Action{Type: ActionListTasks, Project: "p1", Status: "queued"}
	res, err := ExecuteAction(context.Background(), action, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false, want true; msg=%q", res.Message)
	}
	if !strings.Contains(res.Message, "Found 2 task(s)") {
		t.Errorf("Message missing summary: %q", res.Message)
	}
}

func TestExecuteAction_ListTasks_EmptyReturnsFriendlyMessage(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionListTasks}, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success || res.Message != "No tasks found." {
		t.Errorf("empty: success=%v msg=%q", res.Success, res.Message)
	}
}

func TestExecuteAction_ListTasks_RepoError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db down")
		},
	}
	res, err := ExecuteAction(context.Background(), Action{Type: ActionListTasks}, repo, nil, nil)
	if err == nil {
		t.Fatal("expected error from repo, got nil")
	}
	if res.Success {
		t.Error("Success must be false on repo error")
	}
}

// ---- create_task -------------------------------------------------------

func TestExecuteAction_CreateTask_MissingProject(t *testing.T) {
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Type_: "shell"},
		&mocks.MockTaskRepository{}, nil, nil)
	if err == nil || res.Success {
		t.Errorf("missing project: want error+!success, got err=%v success=%v",
			err, res.Success)
	}
}

func TestExecuteAction_CreateTask_MissingType(t *testing.T) {
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1"},
		&mocks.MockTaskRepository{}, nil, nil)
	if err == nil || res.Success {
		t.Errorf("missing type: want error+!success, got err=%v success=%v",
			err, res.Success)
	}
}

func TestExecuteAction_CreateTask_HappyPath(t *testing.T) {
	var created *persistence.Task
	repo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, task *persistence.Task) error {
			created = task
			return nil
		},
	}
	action := Action{
		Type:       ActionCreateTask,
		Project:    "p1",
		Type_:      "shell",
		Priority:   25,
		WorkflowID: "wf-1",
		Input:      map[string]any{"command": "echo hi"},
	}
	res, err := ExecuteAction(context.Background(), action, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success=false; msg=%q", res.Message)
	}
	if created == nil {
		t.Fatal("repo.Create was not called")
	}
	if created.ProjectID != "p1" || created.Priority != 25 {
		t.Errorf("created.{ProjectID,Priority} = %s/%d", created.ProjectID, created.Priority)
	}
	if created.WorkflowID == nil || *created.WorkflowID != "wf-1" {
		t.Errorf("created.WorkflowID = %v, want wf-1", created.WorkflowID)
	}
	if len(created.Payload) == 0 {
		t.Error("created.Payload empty — input marshal lost")
	}
}

// TestExecuteAction_CreateTask_ChatTurnIDStamped — when the action
// carries a ChatTurnID (set by the dispatcher's create_task tool from
// the per-turn context), the resulting task row must reference it.
// Empty ChatTurnID leaves the task pointer nil so non-chat callers
// (API, autonomy) don't accidentally claim a turn.
func TestExecuteAction_CreateTask_ChatTurnIDStamped(t *testing.T) {
	var created *persistence.Task
	repo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, task *persistence.Task) error {
			created = task
			return nil
		},
	}
	turn := "chat_20260521190824_aaaa"
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell", ChatTurnID: turn},
		repo, nil, nil)
	if err != nil || !res.Success {
		t.Fatalf("ExecuteAction: success=%v err=%v", res.Success, err)
	}
	if created == nil {
		t.Fatal("repo.Create not called")
	}
	if created.ChatTurnID == nil || *created.ChatTurnID != turn {
		t.Errorf("ChatTurnID = %v, want %s", created.ChatTurnID, turn)
	}

	// Empty ChatTurnID → nil pointer.
	created = nil
	res, err = ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		repo, nil, nil)
	if err != nil || !res.Success || created == nil {
		t.Fatalf("second create failed")
	}
	if created.ChatTurnID != nil {
		t.Errorf("empty ChatTurnID should leave task.ChatTurnID nil, got %v", created.ChatTurnID)
	}
}

func TestExecuteAction_CreateTask_DefaultsPriorityTo50(t *testing.T) {
	var created *persistence.Task
	repo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, task *persistence.Task) error {
			created = task
			return nil
		},
	}
	res, _ := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		repo, nil, nil)
	if !res.Success || created == nil {
		t.Fatalf("create with default priority: success=%v created=%v",
			res.Success, created)
	}
	if created.Priority != 50 {
		t.Errorf("default priority: got %d, want 50", created.Priority)
	}
}

func TestExecuteAction_CreateTask_RepoFailure(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		CreateFunc: func(context.Context, *persistence.Task) error {
			return errors.New("constraint violation")
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCreateTask, Project: "p1", Type_: "shell"},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("expected failure: err=%v success=%v", err, res.Success)
	}
}

// ---- cancel_task -------------------------------------------------------

func TestExecuteAction_CancelTask_MissingID(t *testing.T) {
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, Confirm: true},
		&mocks.MockTaskRepository{}, nil, nil)
	if err == nil || res.Success {
		t.Errorf("missing task_id: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_CancelTask_HappyPath(t *testing.T) {
	var updatedStatus persistence.TaskStatus
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusQueued}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, s persistence.TaskStatus) error {
			updatedStatus = s
			return nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, TaskID: "t-1", Confirm: true},
		repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success=false; msg=%q", res.Message)
	}
	if updatedStatus != persistence.TaskStatusCancelled {
		t.Errorf("UpdateStatus arg: got %s, want CANCELLED", updatedStatus)
	}
}

func TestExecuteAction_CancelTask_AlreadyTerminal(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, TaskID: "t-1", Confirm: true},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("terminal task: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_CancelTask_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, TaskID: "ghost", Confirm: true},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("not found: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_CancelTask_GetError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, errors.New("db gone")
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, TaskID: "t-1", Confirm: true},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("get error: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_CancelTask_UpdateError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
		UpdateStatusFunc: func(context.Context, string, persistence.TaskStatus) error {
			return errors.New("conflict")
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionCancelTask, TaskID: "t-1", Confirm: true},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("update error: err=%v success=%v", err, res.Success)
	}
}

// ---- retry_task --------------------------------------------------------

func TestExecuteAction_RetryTask_MissingID(t *testing.T) {
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionRetryTask, Confirm: true},
		&mocks.MockTaskRepository{}, nil, nil)
	if err == nil || res.Success {
		t.Errorf("missing id: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_RetryTask_HappyPath(t *testing.T) {
	last := ""
	var updated *persistence.Task
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			last = "got"
			return &persistence.Task{
				ID:        id,
				Status:    persistence.TaskStatusFailed,
				Attempt:   3,
				LastError: stringPtr("boom"),
			}, nil
		},
		UpdateFunc: func(_ context.Context, task *persistence.Task) error {
			updated = task
			return nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionRetryTask, TaskID: "t-1", Confirm: true}, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success=false; msg=%q", res.Message)
	}
	if last != "got" {
		t.Error("GetFunc not invoked")
	}
	if updated == nil {
		t.Fatal("UpdateFunc not invoked")
	}
	if updated.Status != persistence.TaskStatusQueued {
		t.Errorf("status: got %s, want QUEUED", updated.Status)
	}
	if updated.Attempt != 0 {
		t.Errorf("attempt: got %d, want 0", updated.Attempt)
	}
	if updated.LastError != nil {
		t.Errorf("LastError must be cleared on retry, got %v", *updated.LastError)
	}
}

func TestExecuteAction_RetryTask_WrongStatus(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionRetryTask, TaskID: "t-1", Confirm: true}, repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("not FAILED: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_RetryTask_NotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionRetryTask, TaskID: "ghost", Confirm: true}, repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("not found: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_RetryTask_UpdateError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusFailed}, nil
		},
		UpdateFunc: func(context.Context, *persistence.Task) error {
			return errors.New("constraint")
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionRetryTask, TaskID: "t-1", Confirm: true}, repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("update error: err=%v success=%v", err, res.Success)
	}
}

// ---- get_status --------------------------------------------------------

func TestExecuteAction_GetStatus_MissingID(t *testing.T) {
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus},
		&mocks.MockTaskRepository{}, nil, nil)
	if err == nil || res.Success {
		t.Errorf("missing id: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_GetStatus_HappyPath_NoExecRepo(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			payload := []byte(`{"context":{"prompt":"do the thing"}}`)
			return &persistence.Task{
				ID: id, ProjectID: "p1",
				Status:   persistence.TaskStatusRunning,
				Priority: 50, Attempt: 1, MaxAttempts: 3,
				Payload: payload,
			}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1"},
		repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success=false")
	}
	if !strings.Contains(res.Message, "do the thing") {
		t.Errorf("missing prompt in summary: %q", res.Message)
	}
}

func TestExecuteAction_GetStatus_TaskNotFound(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "ghost"},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("not found: err=%v success=%v", err, res.Success)
	}
}

func TestExecuteAction_GetStatus_GetError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return nil, errors.New("conn refused")
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1"},
		repo, nil, nil)
	if err == nil || res.Success {
		t.Errorf("get error: err=%v success=%v", err, res.Success)
	}
}

func stringPtr(s string) *string { return &s }
