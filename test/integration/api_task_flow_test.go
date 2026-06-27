//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/scheduler"
)

// catchAllWorkflowResolver returns a default project/swarm/workflow for any
// ID so that integration tests can run without full YAML config files.
type catchAllWorkflowResolver struct{}

func (r *catchAllWorkflowResolver) GetProject(id string) *registry.Project {
	return &registry.Project{ID: id, SwarmID: "test-swarm", DefaultWorkflowID: "test-wf"}
}
func (r *catchAllWorkflowResolver) GetSwarm(string) *registry.Swarm {
	return &registry.Swarm{
		ID: "test-swarm",
		Roles: []registry.SwarmRole{
			{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
			{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"}},
		},
	}
}
func (r *catchAllWorkflowResolver) GetWorkflow(string) *registry.Workflow {
	return &registry.Workflow{
		ID:         "test-wf",
		Entrypoint: "run",
		Steps: map[string]registry.WorkflowStep{
			"run": {Type: "agent", Role: "lead", Prompt: "run", OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
}

type fakeRuntimeManager struct {
	mu      sync.Mutex
	runs    map[string]*fakeRun
	counter int
}

type fakeRun struct {
	done        chan struct{}
	code        int
	containerID string
	taskID      string
}

func newFakeRuntimeManager() *fakeRuntimeManager {
	return &fakeRuntimeManager{
		runs: make(map[string]*fakeRun),
	}
}

func (m *fakeRuntimeManager) StartContainer(_ context.Context, cfg *runtime.ContainerConfig) (string, error) {
	m.mu.Lock()
	m.counter++
	containerID := fmt.Sprintf("fake-container-%d", m.counter)
	run := &fakeRun{
		done:        make(chan struct{}),
		code:        0,
		containerID: containerID,
		taskID:      cfg.TaskID,
	}
	m.runs[containerID] = run
	m.mu.Unlock()

	go func() {
		defer close(run.done)

		_ = os.MkdirAll(filepath.Join(cfg.WorkspaceDir, "artifacts", "out"), 0o755)
		_ = os.WriteFile(
			filepath.Join(cfg.WorkspaceDir, "artifacts", "out", "result.txt"),
			[]byte("integration test result\n"),
			0o644,
		)
		_ = os.WriteFile(
			filepath.Join(cfg.OutputDir, "result.json"),
			[]byte(`{"status":"COMPLETED","message":"integration fake runtime completed"}`),
			0o644,
		)
	}()

	return containerID, nil
}

func (m *fakeRuntimeManager) StopContainer(_ context.Context, containerID string, _ bool) error {
	m.mu.Lock()
	run, ok := m.runs[containerID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-run.done:
	default:
		close(run.done)
	}
	return nil
}

func (m *fakeRuntimeManager) InspectContainer(_ context.Context, containerID string) (*runtime.Container, error) {
	m.mu.Lock()
	run, ok := m.runs[containerID]
	m.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return &runtime.Container{ID: containerID, TaskID: run.taskID, Status: runtime.StatusRunning}, nil
}

func (m *fakeRuntimeManager) WaitForExit(ctx context.Context, containerID string, _ time.Duration) (int, error) {
	m.mu.Lock()
	run, ok := m.runs[containerID]
	m.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("unknown container: %s", containerID)
	}

	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-run.done:
		return run.code, nil
	}
}

func (m *fakeRuntimeManager) RemoveContainer(_ context.Context, containerID string, _ bool) error {
	m.mu.Lock()
	delete(m.runs, containerID)
	m.mu.Unlock()
	return nil
}

func (m *fakeRuntimeManager) Logs(_ context.Context, _ string, _ int) (string, error) {
	return "", nil
}

func (m *fakeRuntimeManager) GetContainerByTask(_ context.Context, taskID string) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for containerID, run := range m.runs {
		if run.taskID == taskID {
			return &runtime.Container{ID: containerID, TaskID: taskID, Status: runtime.StatusRunning}, nil
		}
	}
	return nil, nil
}

func TestAPI_SubmitTask_AndRetrieveCompletedResult(t *testing.T) {
	db := connectDB(t)

	projectID := fmt.Sprintf("itest-project-%d", time.Now().UnixNano())
	artifactDir := t.TempDir()

	taskRepo := postgres.NewTaskRepository(db)
	execRepo := postgres.NewExecutionRepository(db)
	artifactRepo := postgres.NewArtifactRepository(db)
	artifactStore, err := artifacts.New(
		artifacts.WithBasePath(artifactDir),
		artifacts.WithRepository(artifactRepo),
	)
	require.NoError(t, err)

	schedulerCfg := scheduler.DefaultConfig()
	schedulerCfg.PollInterval = 25 * time.Millisecond
	schedulerCfg.RecoveryInterval = time.Second
	schedulerCfg.ExecutionTimeout = 2 * time.Second
	rt := newFakeRuntimeManager()
	exec := executor.NewWithOptions(
		rt,
		execRepo,
		artifactRepo,
		taskRepo,
		executor.DefaultConfig(),
		executor.WithArtifactStore(artifactStore),
		executor.WithWorkflowResolver(&catchAllWorkflowResolver{}),
	)

	s := scheduler.NewWithOptions(
		taskRepo,
		schedulerCfg,
		scheduler.WithLogger(zerolog.Nop()),
		scheduler.WithRuntimeManager(rt),
		scheduler.WithExecutionRepository(execRepo),
		scheduler.WithExecutor(exec),
		scheduler.WithArtifactStore(artifactStore),
	)
	require.NoError(t, s.Start())
	defer s.Stop()

	cfg := config.DefaultConfig()
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"test-api-key-12345"}

	server := api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithConfig(cfg),
		api.WithTaskRepository(taskRepo),
		api.WithExecutionRepository(execRepo),
		api.WithArtifactRepository(artifactRepo),
	)
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	t.Cleanup(func() {
		cleanupIntegrationProject(t, db, projectID)
	})

	reqBody := []byte(`{"taskType":"integration-test","priority":50}`)
	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/projects/"+projectID+"/tasks", bytes.NewReader(reqBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "test-api-key-12345")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	var createResp struct {
		TaskID string `json:"taskId"`
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&createResp))
	require.NotEmpty(t, createResp.TaskID)
	require.Equal(t, "QUEUED", createResp.Status)

	var lastObserved string
	var dbTaskStatus string
	var dbTaskLastError sql.NullString
	var latestExecution struct {
		ID           string
		Status       string
		ErrorMessage sql.NullString
		ErrorCode    sql.NullString
	}

	terminal := false
	require.Eventually(t, func() bool {
		if err := db.QueryRow(`
			SELECT status, last_error
			FROM tasks
			WHERE id = $1
		`, createResp.TaskID).Scan(&dbTaskStatus, &dbTaskLastError); err != nil {
			lastObserved = fmt.Sprintf("task query failed: %v", err)
			return false
		}

		lastObserved = fmt.Sprintf("status=%s lastError=%q", dbTaskStatus, dbTaskLastError.String)

		if err := db.QueryRow(`
			SELECT id, status, error_message, error_code
			FROM executions
			WHERE task_id = $1
			ORDER BY created_at DESC
			LIMIT 1
		`, createResp.TaskID).Scan(
			&latestExecution.ID,
			&latestExecution.Status,
			&latestExecution.ErrorMessage,
			&latestExecution.ErrorCode,
		); err != nil && err != sql.ErrNoRows {
			lastObserved = fmt.Sprintf("execution query failed: %v", err)
			return false
		}

		terminal = dbTaskStatus == "COMPLETED" || dbTaskStatus == "FAILED"
		return terminal
	}, 20*time.Second, 50*time.Millisecond)
	require.Truef(t, terminal, "task never reached terminal state: %s", lastObserved)
	require.Equalf(
		t,
		"COMPLETED",
		dbTaskStatus,
		"unexpected terminal task state: %s execution_status=%q execution_error=%q execution_code=%q",
		lastObserved,
		latestExecution.Status,
		latestExecution.ErrorMessage.String,
		latestExecution.ErrorCode.String,
	)

	getReq, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/projects/"+projectID+"/tasks/"+createResp.TaskID, nil)
	require.NoError(t, err)
	getReq.Header.Set("X-API-Key", "test-api-key-12345")

	getResp, err := http.DefaultClient.Do(getReq)
	require.NoError(t, err)
	defer getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	var taskResp struct {
		TaskID    string `json:"taskId"`
		Status    string `json:"status"`
		TaskType  string `json:"taskType"`
		LastError string `json:"lastError,omitempty"`
	}
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&taskResp))

	require.Equal(t, createResp.TaskID, taskResp.TaskID)
	require.Equal(t, "integration-test", taskResp.TaskType)
	require.Empty(t, taskResp.LastError)

	var executionCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM executions WHERE task_id = $1 AND status = 'COMPLETED'`, createResp.TaskID).Scan(&executionCount))
	require.Equal(t, 1, executionCount)

	var artifactPath string
	require.NoError(t, db.QueryRow(`SELECT storage_path FROM artifacts WHERE project_id = $1`, projectID).Scan(&artifactPath))
	content, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(content), "integration test result"))
}

func cleanupIntegrationProject(t *testing.T, db *sql.DB, projectID string) {
	t.Helper()

	if err := db.Ping(); err != nil {
		t.Fatalf("cleanup database unavailable: %v", err)
	}

	_, err := db.Exec(`DELETE FROM artifacts WHERE project_id = $1`, projectID)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM executions WHERE project_id = $1`, projectID)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM tasks WHERE project_id = $1`, projectID)
	require.NoError(t, err)
}
