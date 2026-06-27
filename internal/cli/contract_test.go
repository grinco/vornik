// Package cli provides command-line interface commands for vornik.
package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskAPIContract verifies that API response shapes match CLI expectations.
func TestTaskAPIContract(t *testing.T) {
	t.Run("taskResponse", func(t *testing.T) {
		// Sample API response
		apiResponse := `{
			"taskId": "task-123",
			"status": "RUNNING",
			"projectId": "proj-456",
			"taskType": "backup",
			"priority": 50,
			"createdAt": "2026-04-13T00:00:00Z",
			"lastError": ""
		}`

		var resp taskResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, "task-123", resp.TaskID)
		assert.Equal(t, "RUNNING", resp.Status)
		assert.Equal(t, "proj-456", resp.ProjectID)
		assert.Equal(t, "backup", resp.TaskType)
		assert.Equal(t, 50, resp.Priority)
		assert.Equal(t, "2026-04-13T00:00:00Z", resp.CreatedAt)
	})

	t.Run("listTasksResponse", func(t *testing.T) {
		apiResponse := `{
			"tasks": [
				{
					"taskId": "task-1",
					"status": "QUEUED",
					"projectId": "proj-1",
					"taskType": "test",
					"priority": 50,
					"createdAt": "2026-04-13T00:00:00Z"
				},
				{
					"taskId": "task-2",
					"status": "RUNNING",
					"projectId": "proj-1",
					"taskType": "test",
					"priority": 100,
					"createdAt": "2026-04-13T00:01:00Z"
				}
			],
			"total": 2
		}`

		var resp listTasksResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, 2, resp.Total)
		assert.Len(t, resp.Tasks, 2)
		assert.Equal(t, "task-1", resp.Tasks[0].TaskID)
		assert.Equal(t, "task-2", resp.Tasks[1].TaskID)
	})

	t.Run("cancelTaskResponse", func(t *testing.T) {
		apiResponse := `{
			"taskId": "task-123",
			"status": "CANCELLED",
			"wasRunning": true,
			"cancelledAt": "2026-04-13T00:05:00Z"
		}`

		var resp cancelTaskResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, "task-123", resp.TaskID)
		assert.Equal(t, "CANCELLED", resp.Status)
		assert.True(t, resp.WasRunning)
		assert.Equal(t, "2026-04-13T00:05:00Z", resp.CancelledAt)
	})

	t.Run("retryTaskResponse", func(t *testing.T) {
		apiResponse := `{
			"taskId": "task-123",
			"status": "QUEUED",
			"attempt": 2,
			"retriedAt": "2026-04-13T00:10:00Z"
		}`

		var resp retryTaskResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, "task-123", resp.TaskID)
		assert.Equal(t, "QUEUED", resp.Status)
		assert.Equal(t, 2, resp.Attempt)
		assert.Equal(t, "2026-04-13T00:10:00Z", resp.RetriedAt)
	})
}

// TestExecutionAPIContract verifies that execution API responses match CLI expectations.
func TestExecutionAPIContract(t *testing.T) {
	t.Run("executionResponse", func(t *testing.T) {
		apiResponse := `{
			"executionId": "exec-123",
			"taskId": "task-456",
			"projectId": "proj-789",
			"workflowId": "workflow-1",
			"status": "RUNNING",
			"currentStepId": "step-2",
			"completedSteps": ["step-1"],
			"errorMessage": "",
			"errorCode": "",
			"startedAt": "2026-04-13T00:00:00Z",
			"completedAt": "",
			"duration": "5m30s"
		}`

		var resp executionResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, "exec-123", resp.ExecutionID)
		assert.Equal(t, "task-456", resp.TaskID)
		assert.Equal(t, "proj-789", resp.ProjectID)
		assert.Equal(t, "workflow-1", resp.WorkflowID)
		assert.Equal(t, "RUNNING", resp.Status)
		assert.Equal(t, "step-2", resp.CurrentStepID)
		assert.Equal(t, []string{"step-1"}, resp.CompletedSteps)
		assert.Equal(t, "5m30s", resp.Duration)
	})

	t.Run("executionResponse_with_error", func(t *testing.T) {
		apiResponse := `{
			"executionId": "exec-456",
			"taskId": "task-789",
			"projectId": "proj-1",
			"workflowId": "workflow-1",
			"status": "FAILED",
			"errorMessage": "container exited with code 1",
			"errorCode": "RUNTIME_ERROR",
			"startedAt": "2026-04-13T00:00:00Z",
			"completedAt": "2026-04-13T00:01:00Z",
			"duration": "1m"
		}`

		var resp executionResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, "FAILED", resp.Status)
		assert.Equal(t, "container exited with code 1", resp.ErrorMessage)
		assert.Equal(t, "RUNTIME_ERROR", resp.ErrorCode)
		assert.Equal(t, "1m", resp.Duration)
	})

	t.Run("listExecutionsResponse", func(t *testing.T) {
		apiResponse := `{
			"executions": [
				{
					"executionId": "exec-1",
					"taskId": "task-1",
					"projectId": "proj-1",
					"workflowId": "wf-1",
					"status": "COMPLETED",
					"duration": "30s"
				}
			],
			"total": 1
		}`

		var resp listExecutionsResponse
		err := json.Unmarshal([]byte(apiResponse), &resp)
		require.NoError(t, err)

		assert.Equal(t, 1, resp.Total)
		assert.Len(t, resp.Executions, 1)
		assert.Equal(t, "exec-1", resp.Executions[0].ExecutionID)
		assert.Equal(t, "COMPLETED", resp.Executions[0].Status)
	})
}

// TestAPIErrorContract verifies that API error responses match CLI expectations.
func TestAPIErrorContract(t *testing.T) {
	t.Run("apiError", func(t *testing.T) {
		apiResponse := `{
			"code": "NOT_FOUND",
			"message": "task not found"
		}`

		var apiErr APIError
		err := json.Unmarshal([]byte(apiResponse), &apiErr)
		require.NoError(t, err)

		assert.Equal(t, "NOT_FOUND", apiErr.Code)
		assert.Equal(t, "task not found", apiErr.Message)
	})

	t.Run("apiError_validation", func(t *testing.T) {
		apiResponse := `{
			"code": "VALIDATION_ERROR",
			"message": "status must be one of QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED"
		}`

		var apiErr APIError
		err := json.Unmarshal([]byte(apiResponse), &apiErr)
		require.NoError(t, err)

		assert.Equal(t, "VALIDATION_ERROR", apiErr.Code)
		assert.Contains(t, apiErr.Message, "status must be")
	})
}

// TestRoundTripContract verifies structs can be marshaled and unmarshaled without data loss.
func TestRoundTripContract(t *testing.T) {
	t.Run("taskResponse", func(t *testing.T) {
		original := taskResponse{
			TaskID:    "task-123",
			Status:    "RUNNING",
			ProjectID: "proj-456",
			TaskType:  "backup",
			Priority:  50,
			CreatedAt: "2026-04-13T00:00:00Z",
			LastError: "previous error",
		}

		data, err := json.Marshal(original)
		require.NoError(t, err)

		var restored taskResponse
		err = json.Unmarshal(data, &restored)
		require.NoError(t, err)

		assert.Equal(t, original, restored)
	})

	t.Run("executionResponse", func(t *testing.T) {
		original := executionResponse{
			ExecutionID:    "exec-123",
			TaskID:         "task-456",
			ProjectID:      "proj-789",
			WorkflowID:     "workflow-1",
			Status:         "RUNNING",
			CurrentStepID:  "step-2",
			CompletedSteps: []string{"step-1"},
			StartedAt:      "2026-04-13T00:00:00Z",
			Duration:       "5m",
		}

		data, err := json.Marshal(original)
		require.NoError(t, err)

		var restored executionResponse
		err = json.Unmarshal(data, &restored)
		require.NoError(t, err)

		assert.Equal(t, original, restored)
	})
}
