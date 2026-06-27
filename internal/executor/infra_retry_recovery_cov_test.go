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

// TestInfraRetry_RecoversAfterTransientGatewayFailure covers the
// executeAgentStepWithInfraRetry "recovered after transient failure"
// branch (previously uncovered — the 2026-06-18 sweep noted MockRuntime
// couldn't fail-then-succeed). The agent's first result.json reports a
// transient infra error (a `curl: (7)` connection-refused surfaced by the
// chat-proxy call), which isInfraFailure recognises; the retry layer backs
// off and re-runs the step, and the second result.json succeeds, so the
// workflow completes. Asserts the step ran exactly twice.
func TestInfraRetry_RecoversAfterTransientGatewayFailure(t *testing.T) {
	rt := NewMockRuntime()
	// Attempt 1: agent reports a transient gateway/DNS failure (infra
	// marker). Attempt 2: clean success.
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"curl: (7) Failed to connect to gateway: connection refused"}`,
		`{"status":"COMPLETED","message":"ok"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "w", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
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
	exec := &persistence.Execution{ID: "x-infra", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err, "workflow should complete after the transient failure is retried")
	assert.Equal(t, 2, rt.StartCalls(), "step should run twice: one transient failure + one recovery")
}
