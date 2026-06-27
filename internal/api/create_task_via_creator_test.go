package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// makeTaskCreateRegistry builds the same small project / swarm /
// workflow trio used by the UI form tests so the api package's
// createTaskViaCreator path can exercise the full delegation
// pipeline.
func makeTaskCreateRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	write("swarms/coder.md", `---
swarmId: "task-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`)
	write("workflows/build.md", `---
workflowId: "build"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "coder"
    prompt: "do it"
terminals:
  done:
    status: "COMPLETED"
---
`)
	write("projects/p1.yaml", `projectId: "p1"
displayName: "P1"
swarmId: "task-swarm"
defaultWorkflowId: "build"
defaultPriority: 33
rate_limit:
  tasks_per_minute: 1
`)
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

// TestServer_CreateTask_ViaCreator_HappyPath confirms that when
// WithTaskCreator is wired, CreateTask delegates and returns 202
// with the persisted task ID.
func TestServer_CreateTask_ViaCreator_HappyPath(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
	)

	body := `{"taskType":"research","context":{"prompt":"x"}}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, 1, taskRepo.CallCount.Create)
}

// TestServer_CreateTask_ViaCreator_UnknownProject covers the
// 404 mapping from ReasonProjectNotFound. We have to bypass the
// pre-delegation registry check so the Creator handles it; the
// way to do that with the existing handler is to send a missing
// project ID via the URL — but extractProjectID parses the URL.
// Instead we point the server's registry at one project and then
// hit a different ID; the pre-delegation guard catches it,
// returning 404 the same way. Both paths emit the same status,
// which is the externally-visible contract this test pins.
func TestServer_CreateTask_ViaCreator_UnknownProject(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
	)

	body := `{"taskType":"research"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/ghost/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestServer_CreateTask_ViaCreator_RateLimited confirms the
// limiter-induced reject maps to 429 via the delegation path.
func TestServer_CreateTask_ViaCreator_RateLimited(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	limiter := ratelimit.New()
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
		taskcreate.WithRateLimiter(limiter),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
		WithRateLimiter(limiter),
	)

	body := `{"taskType":"research"}`
	// First call passes.
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// Second call should hit the per-minute cap (1).
	req = httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec = httptest.NewRecorder()
	server.CreateTask(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// TestServer_CreateTask_ViaCreator_BudgetExceeded confirms the
// budget-hard-cap branch maps to 429 BUDGET_EXCEEDED through the
// delegation path.
func TestServer_CreateTask_ViaCreator_BudgetExceeded(t *testing.T) {
	reg := budgetTestRegistry(t, 0, 5.0) // hard cap $5
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(_ context.Context, _ *persistence.Task) error {
			t.Fatal("Create must not be called when budget is exceeded")
			return nil
		},
	}
	llmRepo := &mockLLMUsageRepo{cost: 10.0} // $10 spent — over cap
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
		taskcreate.WithLLMUsageRepository(llmRepo),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithLLMUsageRepository(llmRepo),
		WithTaskCreator(creator),
	)
	body := `{"taskType":"research"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "BUDGET_EXCEEDED")
}

// TestServer_CreateTask_ViaCreator_WithMetricsObservesLimiter
// exercises the rate-limit metrics emission branch inside the
// delegation helper. We pass a metrics instance and confirm the
// path still succeeds (the metric itself is exercised in the
// ratelimit package's own tests).
func TestServer_CreateTask_ViaCreator_WithMetricsObservesLimiter(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{}
	limiter := ratelimit.New()
	// Private registry (not the process-global default) so this test is
	// safe under `go test -count>1` — nil here would re-register on the
	// shared default and panic on the second run.
	metrics := ratelimit.NewMetrics(prometheus.NewRegistry())
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
		taskcreate.WithRateLimiter(limiter),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
		WithRateLimiter(limiter),
		WithRateLimitMetrics(metrics),
	)
	body := `{"taskType":"research"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

// TestServer_CreateTask_ViaCreator_IdempotencyReturns200 confirms
// the duplicate-key recovery path: when an existing task with the
// same idempotency key is found, the response code is 200 (re-use)
// rather than 202 (newly accepted).
func TestServer_CreateTask_ViaCreator_IdempotencyReturns200(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	prior := &mocks.MockTaskRepository{}
	existingCreatedAt := time.Now().Add(-10 * time.Millisecond)
	prior.GetByIdempotencyKeyFunc = func(ctx context.Context, projectID, key string) (*persistence.Task, error) {
		return &persistence.Task{
			ID:        "task_existing",
			ProjectID: projectID,
			Status:    persistence.TaskStatusQueued,
			CreatedAt: existingCreatedAt,
		}, nil
	}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(prior),
		taskcreate.WithProjectRegistry(reg),
	)
	server := NewServer(
		WithTaskRepository(prior),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
	)

	body := `{"taskType":"research","idempotencyKey":"dup-key"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "task_existing")
	assert.Equal(t, 0, prior.CallCount.Create)
}

// TestServer_CreateTask_ViaCreator_PersistenceErrorIs500 confirms
// the internal-error branch wraps and returns 500.
func TestServer_CreateTask_ViaCreator_PersistenceErrorIs500(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	taskRepo := &mocks.MockTaskRepository{
		CreateFunc: func(ctx context.Context, task *persistence.Task) error {
			return errors.New("db down")
		},
	}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
	)

	body := `{"taskType":"research"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestServer_CreateTask_ViaCreator_WorkflowIncompat maps to 400
// when the requested workflow can't run on the project's swarm.
func TestServer_CreateTask_ViaCreator_WorkflowIncompat(t *testing.T) {
	reg := makeTaskCreateRegistry(t)
	// Add a second workflow with a missing role.
	dir := reg.GetConfigDir()
	if dir != "" {
		// Append a review workflow that needs "reviewer" — swarm
		// only has "coder", so it's incompat.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "review.md"),
			[]byte(`---
workflowId: "review"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "reviewer"
    prompt: "review"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))
		require.NoError(t, reg.Load(dir))
	}

	taskRepo := &mocks.MockTaskRepository{}
	creator := taskcreate.New(
		taskcreate.WithTaskRepository(taskRepo),
		taskcreate.WithProjectRegistry(reg),
	)
	server := NewServer(
		WithTaskRepository(taskRepo),
		WithProjectRegistry(reg),
		WithTaskCreator(creator),
	)

	body := `{"taskType":"research","workflowId":"review"}`
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.CreateTask(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}
