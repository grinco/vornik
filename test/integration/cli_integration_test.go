//go:build integration
// +build integration

package integration_test

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/cli"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/postgres"
)

// TestCLI_TaskLifecycle tests the full CLI flow for task operations.
func TestCLI_TaskLifecycle(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	projectID := fmt.Sprintf("cli-task-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-cli-%d", time.Now().UnixNano())

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

	t.Cleanup(func() { cleanupIntegrationProject(t, db, projectID) })

	client := cli.NewClient(httpServer.URL, "test-api-key-12345")

	t.Run("list_tasks", func(t *testing.T) {
		resp, err := client.Get("/api/v1/projects/" + projectID + "/tasks")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	})

	t.Run("cancel_task", func(t *testing.T) {
		resp, err := client.Post("/api/v1/projects/"+projectID+"/tasks/"+taskID+"/cancel", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	})

	t.Run("retry_task", func(t *testing.T) {
		resp, err := client.Post("/api/v1/projects/"+projectID+"/tasks/"+taskID+"/retry", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 202, resp.StatusCode)
	})
}

// TestCLI_ExecutionLifecycle tests the full CLI flow for execution operations.
func TestCLI_ExecutionLifecycle(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	projectID := fmt.Sprintf("cli-exec-%d", time.Now().UnixNano())
	taskID := fmt.Sprintf("task-exec-cli-%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec-cli-%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO tasks (id, project_id, status, priority, creation_source, attempt, max_attempts, created_at, updated_at)
		VALUES ($1, $2, 'RUNNING', 50, 'USER', 1, 3, NOW(), NOW())
	`, taskID, projectID)
	require.NoError(t, err)

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

	t.Cleanup(func() { cleanupIntegrationProject(t, db, projectID) })

	client := cli.NewClient(httpServer.URL, "test-api-key-12345")

	t.Run("list_executions", func(t *testing.T) {
		resp, err := client.Get("/api/v1/projects/" + projectID + "/executions")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	})

	t.Run("inspect_execution", func(t *testing.T) {
		resp, err := client.Get("/api/v1/executions/" + execID)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
	})
}
