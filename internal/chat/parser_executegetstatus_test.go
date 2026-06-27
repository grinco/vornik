// Coverage for executeGetStatus's execution-repo branches that
// parser_execute_test.go skipped (LastError, exec.ErrorMessage,
// exec.Result truncation). Lifts the function from 62.9% to ~95%.

package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestExecuteGetStatus_WithLastError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			le := "previous run crashed"
			return &persistence.Task{
				ID: id, Status: persistence.TaskStatusFailed,
				Attempt: 3, MaxAttempts: 3,
				LastError: &le,
			}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1"}, repo, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !res.Success || !strings.Contains(res.Message, "Last Error: previous run crashed") {
		t.Errorf("LastError missing from summary: %q", res.Message)
	}
}

func TestExecuteGetStatus_WithExecution(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	resultMsg := map[string]string{"message": "completed work"}
	resultBytes, _ := json.Marshal(resultMsg)
	emsg := "warning: low fuel"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{
				ID:           "exec-1",
				Status:       persistence.ExecutionStatusCompleted,
				ErrorMessage: &emsg,
				Result:       resultBytes,
			}, nil
		},
	}
	res, err := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1"}, taskRepo, execRepo, nil)
	if err != nil {
		t.Fatalf("ExecuteAction: %v", err)
	}
	if !strings.Contains(res.Message, "Execution: exec-1") {
		t.Errorf("execution ID missing: %q", res.Message)
	}
	if !strings.Contains(res.Message, "completed work") {
		t.Errorf("execution result message missing: %q", res.Message)
	}
	if !strings.Contains(res.Message, "Error: warning: low fuel") {
		t.Errorf("execution error missing: %q", res.Message)
	}
}

func TestExecuteGetStatus_WithExecution_TruncatesLongResult(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	long := strings.Repeat("a", 1000)
	resultBytes, _ := json.Marshal(map[string]string{"message": long})
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{Result: resultBytes}, nil
		},
	}
	res, _ := ExecuteAction(context.Background(),
		Action{Type: ActionGetStatus, TaskID: "t-1"}, taskRepo, execRepo, nil)
	if !strings.Contains(res.Message, "(truncated)") {
		t.Errorf("long result not truncated: %q", res.Message)
	}
}
