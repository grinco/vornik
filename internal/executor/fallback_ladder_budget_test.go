package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// THE FALLBACK HOP'S LADDER
// (design 2026-09-04-fallback-ladder-and-slow-model-breaker §2).
//
// executeAgentStepWithFallback re-ran the step through the SAME wrapper the
// primary used, so the fallback model got its own 6-attempt infra ladder. The
// shipped circuit-breaker design says the opposite, in arithmetic: "~5
// container starts (vs ~12 today: 6 primary ladder + 6 fallback ladder)".
// Twelve is what happened.
//
// Measured over thirty days across ingest / adaptive / research /
// plan-and-write: ZERO recoveries on any *_model_fallback_infra_retryN rung in
// ~66 attempts — about 1.5 hours of wall clock on a rung that has never
// recovered anything. By the time the fallback runs, the step has already
// absorbed a full primary ladder's worth of waiting, so a transient that was
// going to clear has had its chance.
func TestFallbackHop_GetsOneInfraAttempt(t *testing.T) {
	rt := NewMockRuntime()
	// Every call fails with an infra-shaped error: the primary burns its
	// ladder, the fallback must NOT burn a second one. outputJSON (not the
	// sequence) so it repeats for every launch.
	rt.outputJSON = `{"status":"FAILED","message":"PROVIDER_ERROR: upstream provider returned an error"}`
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	exec := &persistence.Execution{ID: "x-fallback-budget", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err, "every call fails, so the step must fail")

	models := rt.LLMModelsLaunched()
	primary, fallback := 0, 0
	for _, m := range models {
		switch m {
		case "primary-model":
			primary++
		case "backup-model":
			fallback++
		}
	}
	assert.Equal(t, 1, fallback,
		"the fallback hop must get exactly ONE attempt — it ran %d. Its infra ladder has "+
			"never recovered a step in production (0 of ~66 attempts over 30 days) and each "+
			"attempt costs a full model timeout. Launches: %v", fallback, models)
	assert.Greater(t, primary, 1,
		"the PRIMARY's ladder is unchanged and must still retry; a change that collapses "+
			"both budgets fails here. Launches: %v", models)
}

// The fallback's SHAPE retry is the opposite case and stays: it changes the
// prompt, so it is a different call, and in production it recovers
// (ingest_model_fallback_shape_retry ok=7, research ok=2).
func TestFallbackHop_KeepsItsShapeRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		// Primary: model-shaped, non-infra, non-shape → straight to fallback.
		`{"status":"FAILED","message":"Tool iteration limit reached"}`,
		// Fallback attempt 1: missing the role's required key → shape failure.
		`{"status":"COMPLETED","message":"forgot the writing object"}`,
		// Fallback shape retry (a DIFFERENT prompt): complete.
		`{"status":"COMPLETED","message":"ok","writing":{"written":true}}`,
	}
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0

	plan := modelFallbackPlan("primary-model", "backup-model")
	plan.swarm.Roles[0].RequiredOutputKeys = []string{"writing"}
	exec := &persistence.Execution{ID: "x-fallback-shape", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err, "the fallback's shape retry must still be able to rescue the step")

	models := rt.LLMModelsLaunched()
	fallback := 0
	for _, m := range models {
		if m == "backup-model" {
			fallback++
		}
	}
	assert.Equal(t, 2, fallback,
		"the fallback must get its attempt PLUS its shape retry (which changes the prompt, "+
			"so it is a different call). Launches: %v", models)
}
