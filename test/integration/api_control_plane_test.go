//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence/postgres"
)

// TestAPI_CancelTask tests the cancel task endpoint.
func TestAPI_CancelTask(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	projectID := fmt.Sprintf("cancel-test-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-cancel-%d", time.Now().UnixNano())

	// Create a queued task
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'QUEUED', 50, 'USER', 1, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	// Cancel the task
	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/api/v1/projects/"+projectID+"/tasks/"+taskID+"/cancel", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var cancelResp struct {
		TaskID string `json:"taskId"`
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&cancelResp))
	require.Equal(t, taskID, cancelResp.TaskID)
	require.Equal(t, "CANCELLED", cancelResp.Status)

	// Verify task is cancelled in DB
	var status string
	err = db.QueryRow(`SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status)
	require.NoError(t, err)
	require.Equal(t, "CANCELLED", status)
}

// TestAPI_CancelTask_Running tests cancelling a running task with executor.
func TestAPI_CancelTask_Running(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	projectID := fmt.Sprintf("cancel-running-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-running-%d", time.Now().UnixNano())

	// Create a running task
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'RUNNING', 50, 'USER', 1, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	// Create execution record
	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	_, err = db.Exec(`
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'test-workflow', 'v1', 'RUNNING', NOW(), NOW())
	`, execID, taskID, projectID)
	require.NoError(t, err)

	// Create executor with mock runtime
	mockRuntime := &mockRuntimeManager{runs: make(map[string]*mockRun)}
	exec := executor.NewWithOptions(
		mockRuntime,
		execRepo,
		nil, // artifactRepo
		taskRepo,
		executor.DefaultConfig(),
	)

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
		api.WithExecutor(exec),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	// Cancel the running task
	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/api/v1/projects/"+projectID+"/tasks/"+taskID+"/cancel", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestAPI_RetryTask tests the retry task endpoint.
func TestAPI_RetryTask(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	projectID := fmt.Sprintf("retry-test-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-retry-%d", time.Now().UnixNano())

	// Create a failed task
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'FAILED', 50, 'USER', 3, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	// Retry the task
	retryBody := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost,
		httpServer.URL+"/api/v1/projects/"+projectID+"/tasks/"+taskID+"/retry", bytes.NewReader(retryBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	var retryResp struct {
		TaskID  string `json:"taskId"`
		Status  string `json:"status"`
		Attempt int    `json:"attempt"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&retryResp))
	require.Equal(t, taskID, retryResp.TaskID)
	require.Equal(t, "QUEUED", retryResp.Status)
	require.Equal(t, 4, retryResp.Attempt) // Incremented from 3

	// Verify task is queued again
	var status string
	err = db.QueryRow(`SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status)
	require.NoError(t, err)
	require.Equal(t, "QUEUED", status)
}

// TestAPI_GetExecution tests the get execution endpoint.
func TestAPI_GetExecution(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	projectID := fmt.Sprintf("exec-test-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-exec-%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())

	// Create a task
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'RUNNING', 50, 'USER', 1, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

	// Create an execution
	_, err = db.Exec(`
		INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'test-workflow', 'v1', 'RUNNING', NOW(), NOW())
	`, execID, taskID, projectID)
	require.NoError(t, err)

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	// Get the execution
	req, err := http.NewRequest(http.MethodGet,
		httpServer.URL+"/api/v1/executions/"+execID, nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var execResp struct {
		ExecutionID string `json:"executionId"`
		TaskID      string `json:"taskId"`
		ProjectID   string `json:"projectId"`
		Status      string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&execResp))
	require.Equal(t, execID, execResp.ExecutionID)
	require.Equal(t, taskID, execResp.TaskID)
	require.Equal(t, projectID, execResp.ProjectID)
	require.Equal(t, "RUNNING", execResp.Status)
}

// TestAPI_ListExecutions tests the list executions endpoint.
func TestAPI_ListExecutions(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	projectID := fmt.Sprintf("list-exec-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-list-%d", time.Now().UnixNano())

	// Create a task
	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'COMPLETED', 50, 'USER', 1, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

	// Create multiple executions
	for i := 0; i < 3; i++ {
		execID := fmt.Sprintf("exec-%d-%d", time.Now().UnixNano(), i)
		status := "COMPLETED"
		if i == 1 {
			status = "FAILED"
		}
		_, err = db.Exec(`
			INSERT INTO executions (id, task_id, project_id, workflow_id, workflow_revision, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'test-workflow', 'v1', $4, NOW(), NOW())
		`, execID, taskID, projectID, status)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond) // Ensure different timestamps
	}

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	// List all executions for project
	req, err := http.NewRequest(http.MethodGet,
		httpServer.URL+"/api/v1/projects/"+projectID+"/executions", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var listResp struct {
		Executions []struct {
			ExecutionID string `json:"executionId"`
			TaskID      string `json:"taskId"`
			Status      string `json:"status"`
		} `json:"executions"`
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listResp))
	require.Equal(t, 3, listResp.Total)

	// Test filtering by status
	req2, err := http.NewRequest(http.MethodGet,
		httpServer.URL+"/api/v1/projects/"+projectID+"/executions?status=FAILED", nil)
	require.NoError(t, err)
	req2.Header.Set("X-API-Key", "test-api-key-12345")

	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	require.Equal(t, http.StatusOK, resp2.StatusCode)

	var filteredResp struct {
		Executions []struct {
			Status string `json:"status"`
		} `json:"executions"`
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&filteredResp))
	require.Equal(t, 1, filteredResp.Total)
	require.Equal(t, "FAILED", filteredResp.Executions[0].Status)
}
