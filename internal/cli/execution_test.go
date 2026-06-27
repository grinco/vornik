package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestExecutionList(t *testing.T) {
	tests := []struct {
		name         string
		projectID    string
		taskID       string
		status       string
		mockResponse interface{}
		mockStatus   int
		expectError  bool
	}{
		{
			name:      "list all executions",
			projectID: "proj_123",
			mockResponse: listExecutionsResponse{
				Executions: []executionResponse{
					{
						ExecutionID: "exec_1",
						TaskID:      "task_1",
						ProjectID:   "proj_123",
						WorkflowID:  "workflow_1",
						Status:      string(persistence.ExecutionStatusRunning),
						Duration:    "1m30s",
					},
				},
				Total: 1,
			},
			mockStatus: http.StatusOK,
		},
		{
			name:      "list with task filter",
			projectID: "proj_123",
			taskID:    "task_1",
			mockResponse: listExecutionsResponse{
				Executions: []executionResponse{},
				Total:      0,
			},
			mockStatus: http.StatusOK,
		},
		{
			name:      "list with status filter",
			projectID: "proj_123",
			status:    string(persistence.ExecutionStatusCompleted),
			mockResponse: listExecutionsResponse{
				Executions: []executionResponse{},
				Total:      0,
			},
			mockStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify path
				if r.URL.Path != "/api/v1/projects/proj_123/executions" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}

				// Verify filters
				if tt.taskID != "" {
					taskID := r.URL.Query().Get("taskId")
					if taskID != tt.taskID {
						t.Errorf("expected taskId filter %s, got %s", tt.taskID, taskID)
					}
				}
				if tt.status != "" {
					status := r.URL.Query().Get("status")
					if status != tt.status {
						t.Errorf("expected status filter %s, got %s", tt.status, status)
					}
				}

				w.WriteHeader(tt.mockStatus)
				_ = json.NewEncoder(w).Encode(tt.mockResponse)
			}))
			defer testServer.Close()

			client := NewClient(testServer.URL, "test-key")

			// Build URL
			path := "/api/v1/projects/proj_123/executions"
			params := []string{}
			if tt.taskID != "" {
				params = append(params, "taskId="+tt.taskID)
			}
			if tt.status != "" {
				params = append(params, "status="+tt.status)
			}
			if len(params) > 0 {
				path += "?" + params[0]
				for i := 1; i < len(params); i++ {
					path += "&" + params[i]
				}
			}

			resp, err := client.Get(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status 200, got %d", resp.StatusCode)
			}

			var result listExecutionsResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
		})
	}
}

func TestExecutionInspect(t *testing.T) {
	tests := []struct {
		name         string
		executionID  string
		mockResponse interface{}
		mockStatus   int
		expectError  bool
	}{
		{
			name:        "inspect running execution",
			executionID: "exec_1",
			mockResponse: executionResponse{
				ExecutionID:   "exec_1",
				TaskID:        "task_1",
				ProjectID:     "proj_123",
				WorkflowID:    "workflow_1",
				Status:        string(persistence.ExecutionStatusRunning),
				CurrentStepID: "step_2",
				StartedAt:     "2024-01-01T00:00:00Z",
				Duration:      "1m30s",
			},
			mockStatus: http.StatusOK,
		},
		{
			name:        "inspect completed execution",
			executionID: "exec_1",
			mockResponse: executionResponse{
				ExecutionID: "exec_1",
				TaskID:      "task_1",
				ProjectID:   "proj_123",
				WorkflowID:  "workflow_1",
				Status:      string(persistence.ExecutionStatusCompleted),
				StartedAt:   "2024-01-01T00:00:00Z",
				CompletedAt: "2024-01-01T00:01:30Z",
				Duration:    "1m30s",
			},
			mockStatus: http.StatusOK,
		},
		{
			name:        "inspect failed execution",
			executionID: "exec_1",
			mockResponse: executionResponse{
				ExecutionID:  "exec_1",
				TaskID:       "task_1",
				ProjectID:    "proj_123",
				WorkflowID:   "workflow_1",
				Status:       string(persistence.ExecutionStatusFailed),
				ErrorMessage: "something went wrong",
				ErrorCode:    "INTERNAL_ERROR",
				StartedAt:    "2024-01-01T00:00:00Z",
				CompletedAt:  "2024-01-01T00:00:30Z",
				Duration:     "30s",
			},
			mockStatus: http.StatusOK,
		},
		{
			name:        "not found",
			executionID: "exec_nonexistent",
			mockStatus:  http.StatusNotFound,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify path
				expectedPath := "/api/v1/executions/" + tt.executionID
				if r.URL.Path != expectedPath {
					t.Errorf("expected path %s, got %s", expectedPath, r.URL.Path)
				}

				w.WriteHeader(tt.mockStatus)
				if tt.mockStatus < 400 {
					_ = json.NewEncoder(w).Encode(tt.mockResponse)
				} else {
					_ = json.NewEncoder(w).Encode(map[string]string{
						"code":    "NOT_FOUND",
						"message": "execution not found",
					})
				}
			}))
			defer testServer.Close()

			client := NewClient(testServer.URL, "test-key")
			resp, err := client.Get("/api/v1/executions/" + tt.executionID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if tt.expectError {
				if resp.StatusCode < 400 {
					t.Error("expected error response")
				}
				return
			}

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status 200, got %d", resp.StatusCode)
			}

			var result executionResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			if result.ExecutionID != tt.executionID {
				t.Errorf("expected execution ID %s, got %s", tt.executionID, result.ExecutionID)
			}
		})
	}
}
