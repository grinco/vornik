package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	keyring "github.com/zalando/go-keyring"
	"vornik.io/vornik/internal/persistence"
)

func TestTaskList(t *testing.T) {
	tests := []struct {
		name           string
		projectID      string
		status         string
		jsonOutput     bool
		mockResponse   interface{}
		mockStatus     int
		expectedOutput string
		expectError    bool
	}{
		{
			name:      "list all tasks",
			projectID: "proj_123",
			mockResponse: listTasksResponse{
				Tasks: []taskResponse{
					{
						TaskID:    "task_1",
						Status:    string(persistence.TaskStatusQueued),
						ProjectID: "proj_123",
						TaskType:  "test.task",
						Priority:  50,
						CreatedAt: "2024-01-01T00:00:00Z",
					},
				},
				Total: 1,
			},
			mockStatus: http.StatusOK,
		},
		{
			name:         "list tasks with status filter",
			projectID:    "proj_123",
			status:       string(persistence.TaskStatusRunning),
			mockResponse: listTasksResponse{Tasks: []taskResponse{}, Total: 0},
			mockStatus:   http.StatusOK,
		},
		{
			name:       "json output",
			projectID:  "proj_123",
			jsonOutput: true,
			mockResponse: listTasksResponse{
				Tasks: []taskResponse{
					{
						TaskID:    "task_1",
						Status:    string(persistence.TaskStatusQueued),
						ProjectID: "proj_123",
						TaskType:  "test.task",
						Priority:  50,
						CreatedAt: "2024-01-01T00:00:00Z",
					},
				},
				Total: 1,
			},
			mockStatus: http.StatusOK,
		},
		{
			name:        "error response",
			projectID:   "proj_123",
			mockStatus:  http.StatusNotFound,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify request path
				if r.URL.Path != "/api/v1/projects/proj_123/tasks" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}

				// Verify status filter
				if tt.status != "" {
					status := r.URL.Query().Get("status")
					if status != tt.status {
						t.Errorf("expected status filter %s, got %s", tt.status, status)
					}
				}

				// Verify API key header
				if r.Header.Get("X-API-Key") != "test-key" {
					t.Error("expected X-API-Key header")
				}

				w.WriteHeader(tt.mockStatus)
				if tt.mockStatus < 400 {
					_ = json.NewEncoder(w).Encode(tt.mockResponse)
				} else {
					_ = json.NewEncoder(w).Encode(map[string]string{
						"code":    "NOT_FOUND",
						"message": "project not found",
					})
				}
			}))
			defer testServer.Close()

			client := NewClient(testServer.URL, "test-key")

			// Build URL
			path := "/api/v1/projects/proj_123/tasks"
			if tt.status != "" {
				path += "?status=" + tt.status
			}

			resp, err := client.Get(path)
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

			if tt.jsonOutput {
				// Test JSON decoding
				var result listTasksResponse
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if result.Total != 1 {
					t.Errorf("expected 1 task, got %d", result.Total)
				}
			}
		})
	}
}

func TestTaskCancel(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify path
		if r.URL.Path != "/api/v1/projects/proj_123/tasks/task_1/cancel" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Verify method
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cancelTaskResponse{
			TaskID:      "task_1",
			Status:      string(persistence.TaskStatusCancelled),
			WasRunning:  false,
			CancelledAt: "2024-01-01T00:00:00Z",
		})
	}))
	defer testServer.Close()

	client := NewClient(testServer.URL, "test-key")
	resp, err := client.Post("/api/v1/projects/proj_123/tasks/task_1/cancel", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var result cancelTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result.TaskID != "task_1" {
		t.Errorf("expected task ID task_1, got %s", result.TaskID)
	}
	if result.Status != string(persistence.TaskStatusCancelled) {
		t.Errorf("expected status CANCELLED, got %s", result.Status)
	}
}

func TestTaskRetry(t *testing.T) {
	tests := []struct {
		name          string
		resetAttempts bool
	}{
		{
			name:          "retry without reset",
			resetAttempts: false,
		},
		{
			name:          "retry with reset",
			resetAttempts: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify path
				if r.URL.Path != "/api/v1/projects/proj_123/tasks/task_1/retry" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}

				// Verify method
				if r.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", r.Method)
				}

				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(retryTaskResponse{
					TaskID:    "task_1",
					Status:    string(persistence.TaskStatusQueued),
					Attempt:   2,
					RetriedAt: "2024-01-01T00:00:00Z",
				})
			}))
			defer testServer.Close()

			client := NewClient(testServer.URL, "test-key")

			var body interface{}
			if tt.resetAttempts {
				body = map[string]bool{"resetAttempts": true}
			}

			resp, err := client.Post("/api/v1/projects/proj_123/tasks/task_1/retry", body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusAccepted {
				t.Errorf("expected status 202, got %d", resp.StatusCode)
			}

			var result retryTaskResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			if result.TaskID != "task_1" {
				t.Errorf("expected task ID task_1, got %s", result.TaskID)
			}
		})
	}
}

func TestClientFromEnv(t *testing.T) {
	// Isolate from the host OS keychain (and any leftover mock state):
	// ClientFromEnv now consults the keychain when VORNIK_API_KEY is
	// unset, so pin an empty in-memory provider for determinism.
	keyring.MockInit()
	_ = DeleteStoredAPIKey()

	// Test with defaults
	_ = os.Unsetenv("VORNIK_API_URL")
	_ = os.Unsetenv("VORNIK_API_KEY")

	client := ClientFromEnv()
	if client.baseURL != DefaultAPIURL {
		t.Errorf("expected default URL %s, got %s", DefaultAPIURL, client.baseURL)
	}
	if client.apiKey != DefaultAPIKey {
		t.Errorf("expected default API key %s, got %s", DefaultAPIKey, client.apiKey)
	}

	// Test with environment variables
	_ = os.Setenv("VORNIK_API_URL", "http://custom-url:9090")
	_ = os.Setenv("VORNIK_API_KEY", "custom-key")
	defer func() { _ = os.Unsetenv("VORNIK_API_URL") }()
	defer func() { _ = os.Unsetenv("VORNIK_API_KEY") }()

	client = ClientFromEnv()
	if client.baseURL != "http://custom-url:9090" {
		t.Errorf("expected custom URL, got %s", client.baseURL)
	}
	if client.apiKey != "custom-key" {
		t.Errorf("expected custom API key, got %s", client.apiKey)
	}
}
