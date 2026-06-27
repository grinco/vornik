package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// TestTaskRetry_FallbackModelButton_AppliesOverride is the UI half of
// the 2026-06-11 "modelFallback steering has no effect" report. The
// "Fallback model" button posts /retry with fallback_model=1; the task
// payload must gain an operator model override switching the swarm's
// fallback-configured roles onto their fallback before the requeue.
// The fixture's edit-swarm has lead.modelFallback=test-lead-fallback.
func TestTaskRetry_FallbackModelButton_AppliesOverride(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	var captured *persistence.Task
	requeued := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{
				ID: id, ProjectID: "p1", Status: persistence.TaskStatusFailed,
				MaxAttempts: 3, Payload: json.RawMessage(`{"context":{"prompt":"x"}}`),
			}, nil
		},
		UpdateFunc:              func(_ context.Context, task *persistence.Task) error { captured = task; return nil },
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) { requeued = true; return true, nil },
	}
	srv := NewServer(WithTaskRepository(repo), WithProjectRegistry(reg))

	form := url.Values{}
	form.Set("fallback_model", "1")
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	srv.TaskRetry(rec, req, "t1")

	assert.True(t, requeued, "task must still be requeued")
	require.NotNil(t, captured, "fallback button must persist a payload override")
	assert.Contains(t, string(captured.Payload), "operator_model_override")
	assert.Contains(t, string(captured.Payload), "test-lead-fallback")
	assert.Contains(t, string(captured.Payload), "prompt", "original payload context must survive")
}

// TestTaskRetry_PlainRequeue_NoOverride — a bare requeue (no
// fallback_model field) must NOT touch the payload.
func TestTaskRetry_PlainRequeue_NoOverride(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	updateCalled := false
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p1", Status: persistence.TaskStatusFailed, MaxAttempts: 3}, nil
		},
		UpdateFunc:              func(_ context.Context, _ *persistence.Task) error { updateCalled = true; return nil },
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) { return true, nil },
	}
	srv := NewServer(WithTaskRepository(repo), WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/retry", nil)
	rec := httptest.NewRecorder()
	srv.TaskRetry(rec, req, "t1")

	assert.False(t, updateCalled, "plain requeue must not rewrite the task payload")
}
