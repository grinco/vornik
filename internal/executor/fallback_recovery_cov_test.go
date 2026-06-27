package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestModelFallback_RecoversAfterProviderError covers executeAgentStepWithFallback's
// model-swap recovery path (the lowest-covered retry layer). The role declares a
// ModelFallback; the agent's first result.json reports a PROVIDER_ERROR (an
// isModelShapedFailure trigger that is neither an infra nor a shape failure, so it
// falls through to the fallback layer), then the retry on the fallback model
// succeeds and the workflow completes.
func TestModelFallback_RecoversAfterProviderError(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"PROVIDER_ERROR: upstream returned 500"}`,
		`{"status":"COMPLETED","message":"ok"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Model: "primary-model", ModelFallback: "backup-model",
				Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID:            "wf",
			Entrypoint:    "step1",
			MaxIterations: 10,
			MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"step1": {Type: "agent", Role: "w", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"done": {Status: "COMPLETED"},
			},
		},
	}
	exec := &persistence.Execution{ID: "x-fallback", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err, "workflow should complete after the model-shaped failure triggers a fallback retry")
	// Recovered: the step ran more than once (primary failed, fallback succeeded).
	assert.GreaterOrEqual(t, rt.StartCalls(), 2, "fallback should re-run the step on the backup model")
}
