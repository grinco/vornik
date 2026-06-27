package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// fallbackHintRegistry seeds a project p1 → swarm fb-swarm whose
// researcher role has a configured modelFallback, so the steer-keyword
// path has something to override.
func fallbackHintRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"), []byte(`projectId: p1
displayName: P1
swarmId: fb-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "fb-swarm.md"), []byte(`---
swarmId: fb-swarm
displayName: FB
leadRole: researcher
roles:
  - name: "researcher"
    description: "Researches"
    model: "gpt-5.4-mini"
    modelFallback: "minimax.minimax-m2.5"
    runtime:
      image: "vornik-agent:latest"
---

# FB

## Role prompts

### researcher

Do research.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "w1.md"), []byte(`---
workflowId: w1
entrypoint: research
steps:
  research:
    type: agent
    prompt: "do work"
    role: researcher
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func fallbackHintServer(t *testing.T, task *persistence.Task, captured **persistence.Task) *Server {
	t.Helper()
	tr := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if task == nil || task.ID != id {
				return nil, persistence.ErrNotFound
			}
			return task, nil
		},
		UpdateFunc: func(_ context.Context, tk *persistence.Task) error { *captured = tk; return nil },
	}
	return NewServer(
		WithTaskRepository(tr),
		WithExecutionHintRepository(&stubHintRepo{}),
		WithProjectRegistry(fallbackHintRegistry(t)),
	)
}

// TestTaskHintCreate_FallbackKeyword_AppliesOverride — a steer hint
// containing "model: fallback" switches the swarm's fallback-configured
// roles onto their fallback by writing the operator override into the
// task payload, so the retry fired right after picks it up.
func TestTaskHintCreate_FallbackKeyword_AppliesOverride(t *testing.T) {
	task := &persistence.Task{ID: "task_xyz", ProjectID: "p1", Payload: json.RawMessage(`{"context":{}}`)}
	var captured *persistence.Task
	srv := fallbackHintServer(t, task, &captured)

	req := hintBody(t, ExecutionHintRequest{Content: "Retry research. model: fallback please, tighter guardrails"})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "task_xyz")

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, captured, "keyword must trigger a payload override write")
	assert.Contains(t, string(captured.Payload), "operator_model_override")
	assert.Contains(t, string(captured.Payload), "minimax.minimax-m2.5")
}

// TestTaskHintCreate_PlainHint_NoOverride — an ordinary steer hint must
// not rewrite the payload.
func TestTaskHintCreate_PlainHint_NoOverride(t *testing.T) {
	task := &persistence.Task{ID: "task_xyz", ProjectID: "p1", Payload: json.RawMessage(`{"context":{}}`)}
	var captured *persistence.Task
	srv := fallbackHintServer(t, task, &captured)

	req := hintBody(t, ExecutionHintRequest{Content: "focus only on the official portal"})
	rec := httptest.NewRecorder()
	srv.TaskHintCreate(rec, req, "p1", "task_xyz")

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Nil(t, captured, "plain hint must not rewrite the task payload")
}
