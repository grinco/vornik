package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/agenthealth"
	"vornik.io/vornik/internal/persistence"
)

// TestAgentHealthBreaker_OpenFastFailsOverToModelFallback is the wired,
// end-to-end test for the executor-side agent-LLM circuit breaker (LLD
// 2026-07-12-agent-llm-health-breaker §9). It pre-trips the breaker for the
// role's PRIMARY model, then drives the real executeWorkflowAttempt loop and
// asserts:
//   - the primary's container is NEVER started (the gate fast-rejects with a
//     *chat.ModelUnhealthyError before the container launch);
//   - that error flows through isModelUnhealthyFailure (skip the infra ladder)
//   - isModelShapedFailure (trigger modelFallback), so the role's
//     modelFallback re-runs on the healthy backup model and the workflow
//     completes.
//
// This is the topology-1 fix (the 2026-07-12 ~12-container-starts incident):
// a sick primary flips to the fallback in seconds, not tens of minutes.
func TestAgentHealthBreaker_OpenFastFailsOverToModelFallback(t *testing.T) {
	rt := NewMockRuntime()
	// The primary never runs (gate rejects), so the only container launch
	// is the fallback re-run, which succeeds.
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	// Pre-trip the agent breaker for the primary model (3 sustained infra
	// failures — MinSamples=3, the agent default).
	reg := agenthealth.NewRegistry(agenthealth.Config{
		Health:  chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second},
		Enabled: true,
	})
	for i := 0; i < 3; i++ {
		reg.Record("primary-model", false, errors.New("PROVIDER_ERROR: upstream provider returned an error"))
	}

	e := NewWithOptions(rt, er, ar, tr, nil, WithAgentHealth(reg))
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-agent-breaker-open", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err,
		"an OPEN primary breaker must fast-fail-over to the fallback model, which succeeds")

	models := rt.LLMModelsLaunched()
	require.Len(t, models, 1,
		"the primary must be fast-rejected (no container); only the fallback re-run launches")
	assert.Equal(t, "backup-model", models[0],
		"the fallback re-run must launch on the configured modelFallback")
	for _, m := range models {
		assert.NotEqual(t, "primary-model", m,
			"the sick primary must NOT have a container started — the gate fast-rejected it")
	}
}

// TestAgentHealthBreaker_DisabledIsPassthrough asserts that with the breaker
// disabled (enabled:false), a normal primary-then-success path is byte-
// identical to pre-breaker behaviour: the primary container launches and the
// workflow completes — no gate interference (LLD §12 B3).
func TestAgentHealthBreaker_DisabledIsPassthrough(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	er := NewMockExecRepo()
	reg := agenthealth.NewRegistry(agenthealth.Config{Enabled: false}) // disabled
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithAgentHealth(reg))
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-agent-breaker-disabled", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err)
	models := rt.LLMModelsLaunched()
	require.Len(t, models, 1, "disabled breaker: single successful step → one launch on the primary")
	assert.Equal(t, "primary-model", models[0])
}

// TestAgentHealthBreaker_NilRegistryIsPassthrough asserts that a nil registry
// (not wired) is a passthrough — the gate is a no-op (LLD §12 B3 nil-safety).
func TestAgentHealthBreaker_NilRegistryIsPassthrough(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil) // no WithAgentHealth
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-agent-breaker-nil", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"primary-model"}, rt.LLMModelsLaunched())
}
