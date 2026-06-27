package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// modelFallbackPlan builds a minimal single-agent workflow whose only role
// declares both a primary `model` and a `modelFallback`. Shared by the wired
// model-fallback tests below so the swarm/workflow shape stays in one place.
func modelFallbackPlan(primary, fallback string) *executionPlan {
	return &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{
				Name:          "w",
				Model:         primary,
				ModelFallback: fallback,
				Runtime:       registry.SwarmRoleRuntime{Image: "img"},
			},
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
}

// TestModelFallback_LaunchesContainerWithFallbackModel is the wired,
// end-to-end half of the in-execution model fallback (https://docs.vornik.io Tier-2
// "LLM egress proxy + model-fallback"). The existing unit tests cover the
// pieces in isolation — isModelShapedFailure classification (retry_test.go),
// RecordModelFallback metrics (metrics_test.go), and that a fallback re-run
// *happens* (fallback_recovery_cov_test.go asserts StartCalls>=2). None of
// them assert WHICH model the executor actually launched the container with on
// the fallback attempt.
//
// This test closes that gap by driving the real executeWorkflowAttempt loop
// against a MockRuntime that records the VORNIK_LLM_MODEL env captured on each
// StartContainer call, then asserting the sequence is
// [primary-model, backup-model]: the primary on the first attempt, the
// configured fallback on the model-fallback re-run.
//
// The trigger is a "Tool iteration limit reached" agent failure — chosen
// deliberately because it is an isModelShapedFailure marker that is NEITHER an
// infra-retry marker (so the cheaper same-model infra retry does not swallow
// it first) NOR a classifyShapeFailure shape (so the same-model shape retry is
// skipped), routing the failure straight to executeAgentStepWithFallback's
// model-swap branch. PROVIDER_ERROR — used by the sibling
// fallback_recovery_cov_test.go — is also an infra marker, so it actually
// recovers via the infra-retry layer on the SAME model and never exercises the
// model swap; that subtlety is why this test exists alongside it.
func TestModelFallback_LaunchesContainerWithFallbackModel(t *testing.T) {
	rt := NewMockRuntime()
	// First agent run fails with a model-shaped, non-infra, non-shape error;
	// the fallback re-run succeeds.
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"Tool iteration limit reached"}`,
		`{"status":"COMPLETED","message":"ok"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-fallback-wired", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err,
		"workflow should complete after the model-shaped failure triggers a fallback retry")

	models := rt.LLMModelsLaunched()
	require.Len(t, models, 2,
		"expected exactly two container launches: primary attempt + fallback re-run")
	assert.Equal(t, "primary-model", models[0],
		"the first attempt must run on the role's primary model")
	assert.Equal(t, "backup-model", models[1],
		"the fallback re-run must launch the container with the configured modelFallback — "+
			"the fallback model has to be the one actually called, not merely the metrics label")
}

// TestModelFallback_NoFallbackConfigured_StaysOnPrimary is the negative
// characterization: a role WITHOUT a modelFallback must never swap models even
// on a model-shaped failure. Guards executeAgentStepWithFallback's
// `roleConfig.ModelFallback == ""` early return — without it a future change
// could start fabricating a fallback target. The single launch stays on the
// primary model and the original error surfaces.
func TestModelFallback_NoFallbackConfigured_StaysOnPrimary(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"Tool iteration limit reached"}`,
	}
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "") // no fallback configured
	exec := &persistence.Execution{ID: "x-no-fallback", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err, "with no fallback and a persistent model-shaped failure the step must fail")

	models := rt.LLMModelsLaunched()
	for i, m := range models {
		assert.Equal(t, "primary-model", m,
			"launch %d must stay on the primary model when no modelFallback is configured", i)
	}
}

// TestModelFallback_OperatorOverrideAppliedAtLaunch is the regression for the
// 2026-06-20 bug: operator_model_override (the "Fallback model" button /
// `model: fallback` steer-hint, and the recovery `model_fallback` action) was
// resolved by effectiveRoleModelForTask but only fed metrics labels — the
// container still launched on the role's PRIMARY model. The override must be
// the model the container is ACTUALLY started with, or the operator/model-health
// mitigation is silently a no-op.
func TestModelFallback_OperatorOverrideAppliedAtLaunch(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-op-override", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	payload, err := WithOperatorModelOverride(json.RawMessage(`{}`), map[string]string{"w": "operator-chosen"})
	require.NoError(t, err)
	task := &persistence.Task{ID: "t", ProjectID: "p", Payload: payload, CreatedAt: time.Now()}

	_, _, _, err = e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err)

	models := rt.LLMModelsLaunched()
	require.Len(t, models, 1, "single successful step → exactly one container launch")
	assert.Equal(t, "operator-chosen", models[0],
		"operator_model_override must be the model the container actually launches with, not the role primary")
}
