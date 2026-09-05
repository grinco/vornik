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

// emptyResultHandler succeeds with no Result — a legal SystemHandler return
// shape (test doubles use it, and the package comment anticipates operator
// plugin handlers), and the one the success path handled asymmetrically.
type emptyResultHandler struct{ name string }

func (h *emptyResultHandler) Name() string { return h.name }
func (h *emptyResultHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	return SystemStepResult{}, nil
}

// The failing half reuses the package's existing failingSystemHandler double
// (system_step_terminal_error_test.go): a second one would be the same object
// under a second name.

// TestSystemStep_EmptySuccessDoesNotCarryThePreviousStepsError — the residual
// half of the asymmetry 9db8b7bb fixed (2026-09-03 four-week audit).
//
// The system-step SUCCESS branch sets lastResultMessage only when the handler
// returned a non-empty Result; the empty-Result branch sets state.LastResult
// to {} but leaves lastResultMessage alone. The agent-step success path resets
// it unconditionally (workflow.go:475), which is what makes this an asymmetry
// rather than a choice.
//
// So: a step fails and routes on_fail into a system step whose handler
// legitimately returns SystemStepResult{}, nil — and the NEXT agent receives
// the failed step's error text as previousStepResult, presented as its
// predecessor's output. A recovery step that quietly succeeds hands the next
// agent the error it just recovered from.
//
// Not reachable through any shipped handler today (all five populate Result on
// success), which is why it is a P3 and not a P1 — but it is a legal return
// shape, and "no shipped caller does this yet" is the kind of guarantee that
// expires the day someone writes a plugin.
func TestSystemStep_EmptySuccessDoesNotCarryThePreviousStepsError(t *testing.T) {
	const errText = "SYSTEM-STEP-FAILURE-MARKER-4b91c7"

	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	rt := &capturingRuntime{MockRuntime: mock}

	reg := NewSystemHandlerRegistry()
	reg.Register(&failingSystemHandler{name: "test.fails", retErr: errTestSystemStep(errText)})
	reg.Register(&emptyResultHandler{name: "test.recovers"})

	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "attempt",
				Steps: map[string]registry.WorkflowStep{
					// Fails → its error text lands in lastResultMessage.
					"attempt": {Type: "system", Handler: "test.fails", OnFail: "recover", OnSuccess: "review"},
					// Succeeds with an EMPTY result → must clear it.
					"recover": {Type: "system", Handler: "test.recovers", OnSuccess: "review", OnFail: "failed"},
					// Sees whatever the recovery step left behind.
					"review": {Type: "agent", Role: "reviewer", Prompt: "Continue.", OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	const taskID = "t-empty-result"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"continue"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(taskID))

	var raw []byte
	for i := 0; i < 200; i++ {
		if raw = rt.latestCapture(); raw != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.NotNil(t, raw, "the agent step must have started a container and been handed a task.json")

	var payload struct {
		Context map[string]any `json:"context"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))

	prev, present := payload.Context["previousStepResult"]
	if present {
		assert.NotContains(t, prev, errText,
			"the recovery step succeeded, so the agent after it must NOT be handed the "+
				"error text of the step that failed BEFORE it — that is another step's "+
				"output, misattributed to its predecessor")
	}
}

// errTestSystemStep keeps the marker text out of a wrapped error's prefix so
// the assertion above is about propagation, not formatting.
type testSystemStepError string

func (e testSystemStepError) Error() string { return string(e) }

func errTestSystemStep(s string) error { return testSystemStepError(s) }
