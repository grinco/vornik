package chat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

func TestParseActions(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     []Action
	}{
		{
			name:     "no actions",
			response: "This is a regular message without any actions.",
			want:     nil,
		},
		{
			name:     "single list_tasks action",
			response: `Here are your tasks: {"action": "list_tasks"}`,
			want: []Action{
				{Type: "list_tasks"},
			},
		},
		{
			name:     "single list_tasks with project filter",
			response: `{"action": "list_tasks", "project": "my-project"}`,
			want: []Action{
				{Type: "list_tasks", Project: "my-project"},
			},
		},
		{
			name:     "create_task action",
			response: `I'll create a task for you: {"action": "create_task", "project": "test-project", "type": "shell", "input": {"command": "echo hello"}}`,
			want: []Action{
				{
					Type:    "create_task",
					Project: "test-project",
					Type_:   "shell",
					Input:   map[string]interface{}{"command": "echo hello"},
				},
			},
		},
		{
			name:     "cancel_task action",
			response: `{"action": "cancel_task", "task_id": "task-123"}`,
			want: []Action{
				{Type: "cancel_task", TaskID: "task-123"},
			},
		},
		{
			name:     "get_status action",
			response: `{"action": "get_status", "task_id": "task-456"}`,
			want: []Action{
				{Type: "get_status", TaskID: "task-456"},
			},
		},
		{
			name:     "multiple actions",
			response: `First: {"action": "list_tasks"}. Second: {"action": "get_status", "task_id": "task-789"}`,
			want: []Action{
				{Type: "list_tasks"},
				{Type: "get_status", TaskID: "task-789"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseActions(tt.response)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			assert.Len(t, got, len(tt.want))
			for i, action := range got {
				if i < len(tt.want) {
					assert.Equal(t, tt.want[i].Type, action.Type)
					assert.Equal(t, tt.want[i].Project, action.Project)
					assert.Equal(t, tt.want[i].TaskID, action.TaskID)
					assert.Equal(t, tt.want[i].Type_, action.Type_)
				}
			}
		})
	}
}

func TestExecuteAction_UnknownAction(t *testing.T) {
	action := Action{Type: "unknown_action"}
	result, err := ExecuteAction(context.Background(), action, nil, nil, nil)
	assert.Error(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "Unknown action")
}

func TestExecuteAction_ListTasks(t *testing.T) {
	// This test would require a mock TaskRepository
	// For now, we test the unknown action path
	t.Skip("requires mock TaskRepository")
}

func TestActionConstants(t *testing.T) {
	// Verify action constants are defined correctly
	assert.Equal(t, "list_tasks", ActionListTasks)
	assert.Equal(t, "create_task", ActionCreateTask)
	assert.Equal(t, "cancel_task", ActionCancelTask)
	assert.Equal(t, "retry_task", ActionRetryTask)
	assert.Equal(t, "get_status", ActionGetStatus)
}

func TestAction_Struct(t *testing.T) {
	action := Action{
		Type:    "create_task",
		Project: "test-project",
		TaskID:  "task-123",
		Status:  "running",
		Type_:   "shell",
		Input: map[string]interface{}{
			"command": "echo test",
		},
	}

	assert.Equal(t, "create_task", action.Type)
	assert.Equal(t, "test-project", action.Project)
	assert.Equal(t, "task-123", action.TaskID)
	assert.Equal(t, "running", action.Status)
	assert.Equal(t, "shell", action.Type_)
	assert.NotNil(t, action.Input)
}

func TestActionResult_Struct(t *testing.T) {
	result := ActionResult{
		Success: true,
		Message: "Task created successfully",
		Data:    &persistence.Task{ID: "task-123"},
	}

	assert.True(t, result.Success)
	assert.Equal(t, "Task created successfully", result.Message)
	assert.NotNil(t, result.Data)
}
