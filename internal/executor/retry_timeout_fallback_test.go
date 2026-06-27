package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// 2026-06-24 regression: a model that times out on EVERY infra-retry attempt
// (zai.glm-5 at 247-564s vs a 120s VORNIK_LLM_TIMEOUT) looped 6× and failed
// without ever trying the configured modelFallback, because the fallback layer
// only fired on "model-shaped" failures and excluded timeouts. The persistent
// (infra-retry-exhausted) timeout must now trigger the fallback.
func TestIsPersistentTimeoutFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"exhausted curl-28 timeout", errors.New("infra retry exhausted after 6 attempts: agent reported FAILED status: LLM call failed: curl failed (exit 28): curl: (28) Operation timed out after 120002 milliseconds with 0 bytes received"), true},
		{"exhausted i/o timeout", errors.New("infra retry exhausted after 6 attempts: i/o timeout"), true},
		{"exhausted context deadline", errors.New("infra retry exhausted after 6 attempts: context deadline exceeded"), true},
		{"single transient timeout, not exhausted", errors.New("curl: (28) Operation timed out"), false},
		{"exhausted but connection refused (not a timeout)", errors.New("infra retry exhausted after 6 attempts: connection refused"), false},
		{"model-shaped provider error (not a timeout)", errors.New("PROVIDER_ERROR: upstream 500"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPersistentTimeoutFailure(tc.err); got != tc.want {
				t.Errorf("isPersistentTimeoutFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

// 2026-06-24 follow-up: timeout-class infra failures are expensive (each one
// burns the full per-call VORNIK_LLM_TIMEOUT, ~120s). Running the full 6-attempt
// infra-retry budget on a structurally-too-slow model delays the model fallback
// by ~13min. Timeout-class failures must therefore exhaust early (after
// infraRetryTimeoutAttempts), while non-timeout infra failures keep the full
// budget.
func TestIsTimeoutInfraFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"curl-28 timed out", errors.New("curl: (28) Operation timed out after 120001 milliseconds with 0 bytes received"), true},
		{"i/o timeout", errors.New("agent reported FAILED status: i/o timeout"), true},
		{"context deadline", errors.New("context deadline exceeded"), true},
		{"no bytes received", errors.New("Operation timed out with 0 bytes received"), true},
		{"connection refused (not a timeout)", errors.New("curl: (7) connection refused"), false},
		{"DNS no such host (not a timeout)", errors.New("curl: (6) no such host"), false},
		{"provider 500 (not a timeout)", errors.New("PROVIDER_ERROR: upstream 500"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTimeoutInfraFailure(tc.err); got != tc.want {
				t.Errorf("isTimeoutInfraFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

// End-to-end: a primary model whose calls all time out (curl exit 28) exhausts
// the infra-retry budget EARLY (after infraRetryTimeoutAttempts, not the full
// infraRetryMaxAttempts), then the step recovers on the configured fallback
// model. Mirrors TestModelFallback_RecoversAfterProviderError but for the
// timeout class that previously never reached the fallback.
func TestModelFallback_RecoversAfterPersistentTimeout(t *testing.T) {
	// Zero the infra-retry backoff so exhaustion runs fast.
	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })

	rt := NewMockRuntime()
	timeoutFail := `{"status":"FAILED","message":"LLM call failed: curl failed (exit 28): curl: (28) Operation timed out after 120002 milliseconds with 0 bytes received"}`
	seq := make([]string, 0, infraRetryTimeoutAttempts+1)
	for i := 0; i < infraRetryTimeoutAttempts; i++ {
		seq = append(seq, timeoutFail) // primary times out up to the timeout cap
	}
	seq = append(seq, `{"status":"COMPLETED","message":"ok"}`) // fallback succeeds
	rt.outputJSONSequence = seq

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Model: "primary-slow", ModelFallback: "backup-fast",
				Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID: "wf", Entrypoint: "step1", MaxIterations: 10, MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"step1": {Type: "agent", Role: "w", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		},
	}
	exec := &persistence.Execution{ID: "x-timeout-fb", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err, "workflow should recover after a persistent timeout triggers model fallback")
	assert.Equal(t, infraRetryTimeoutAttempts+1, rt.StartCalls(),
		"primary should exhaust the timeout budget early (not the full infra-retry budget), then the fallback model runs once")
}

// Non-timeout infra failures (e.g. connection refused during chat-proxy warmup)
// must keep the FULL infra-retry budget — the early-exhaustion shortcut applies
// to timeout-class failures only. Here the primary recovers on attempt
// infraRetryMaxAttempts via plain infra-retry, with no model fallback involved.
func TestInfraRetry_NonTimeoutKeepsFullBudget(t *testing.T) {
	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })

	rt := NewMockRuntime()
	connRefused := `{"status":"FAILED","message":"LLM call failed: curl failed (exit 7): curl: (7) connection refused"}`
	seq := make([]string, 0, infraRetryMaxAttempts)
	for i := 0; i < infraRetryMaxAttempts-1; i++ {
		seq = append(seq, connRefused) // refused on every attempt but the last
	}
	seq = append(seq, `{"status":"COMPLETED","message":"ok"}`) // recovers on the final attempt
	rt.outputJSONSequence = seq

	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Model: "primary", ModelFallback: "backup-fast",
				Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID: "wf", Entrypoint: "step1", MaxIterations: 10, MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"step1": {Type: "agent", Role: "w", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		},
	}
	exec := &persistence.Execution{ID: "x-conn-refused", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err, "workflow should recover via the full infra-retry budget on connection-refused")
	assert.Equal(t, infraRetryMaxAttempts, rt.StartCalls(),
		"connection-refused is non-timeout: it must use the full infra-retry budget, not exhaust early")
}
