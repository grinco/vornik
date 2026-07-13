package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// failingSystemHandler always returns retErr from Execute.
type failingSystemHandler struct {
	name   string
	retErr error
}

func (h *failingSystemHandler) Name() string { return h.name }
func (h *failingSystemHandler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	return SystemStepResult{}, h.retErr
}

// systemStepFailPlan builds a workflow whose entrypoint is a system step that
// fails via OnFail straight to a FAILED terminal. handlerName selects which
// registered handler the step invokes (a registered failing one, or an
// unregistered name to exercise the unknown_handler path).
func systemStepFailPlan(handlerName string) *executionPlan {
	return &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID:            "wf",
			Entrypoint:    "step1",
			MaxIterations: 10,
			MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"step1": {Type: "system", Handler: handlerName, OnFail: "failed"},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"failed": {Status: "FAILED", Message: "workflow failed"},
			},
		},
	}
}

// runSystemStepFail drives a workflow whose entrypoint system step invokes
// "rag.extract" and fails via OnFail to a FAILED terminal, returning the
// terminal error. The caller's registry decides whether "rag.extract" is a
// registered-but-failing handler or unregistered (unknown_handler path).
func runSystemStepFail(t *testing.T, reg *SystemHandlerRegistry) error {
	t.Helper()
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	plan := systemStepFailPlan("rag.extract")
	exec := &persistence.Execution{ID: "x-sysfail", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	return err
}

// TestSystemStepOnFail_TerminalSurfacesHandlerError is the fix (LLD 2026-07-12-
// system-step-handler-error-terminal): a system step that fails via OnFail
// into a FAILED terminal must surface the REAL handler error in the task's
// returned error — not just the generic "workflow failed" + a stale agent
// message. The "system step <handler> failed:" prefix distinguishes a handler
// error from an agent message for operator self-diagnosis.
func TestSystemStepOnFail_TerminalSurfacesHandlerError(t *testing.T) {
	reg := NewSystemHandlerRegistry()
	reg.Register(&failingSystemHandler{
		name:   "rag.extract",
		retErr: errors.New("extraction failed: corrupt PDF"),
	})
	err := runSystemStepFail(t, reg)
	require.Error(t, err, "the workflow must reach a FAILED terminal")

	msg := err.Error()
	assert.Contains(t, msg, "extraction failed: corrupt PDF",
		"the real handler error must appear in the terminal error (was: only the generic 'workflow failed')")
	assert.Contains(t, msg, "system step rag.extract failed:",
		"the reason must carry the handler-error prefix so operators don't mistake it for an agent message")
}

// TestSystemStepUnknownHandler_TerminalSurfacesError — the unknown_handler
// path (a workflow referencing a handler the daemon didn't register) must
// likewise surface a self-diagnosing terminal, not a stale agent message.
func TestSystemStepUnknownHandler_TerminalSurfacesError(t *testing.T) {
	reg := NewSystemHandlerRegistry() // empty — "rag.extract" is unregistered
	err := runSystemStepFail(t, reg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "system step rag.extract failed:",
		"unknown-handler terminal must carry the handler-error prefix")
	assert.Contains(t, strings.ToLower(msg), "no handler registered",
		"the terminal must name the unknown-handler cause")
}

// TestSystemStepOnFail_NoStaleWChain — the fix sets lastResultMessage ONLY
// (no lastResultErr), so the returned error is a flat fmt.Errorf("%s") with no
// wrapped handler-error chain feeding the task-level failure classifier. Pin
// that the handler error is NOT reachable via errors.As of the handler's
// concrete error type (there is no %w wrap).
func TestSystemStepOnFail_NoStaleWChain(t *testing.T) {
	sentinel := &markerError{msg: "extraction failed: sentinel"}
	reg := NewSystemHandlerRegistry()
	reg.Register(&failingSystemHandler{name: "rag.extract", retErr: sentinel})
	err := runSystemStepFail(t, reg)
	require.Error(t, err)
	// The message is surfaced (diagnosability)…
	assert.Contains(t, err.Error(), "extraction failed: sentinel")
	// …but the concrete error is NOT in the chain (no %w — avoids
	// classifier coupling, LLD §10 BLOCKER).
	var me *markerError
	assert.False(t, errors.As(err, &me),
		"handler error must NOT be %w-wrapped into the terminal chain (message-only)")
}

// TestSystemStepOnFail_NoClassificationDrift is the §6/§7 gate: surfacing the
// handler error in the terminal must NOT spuriously reclassify the task
// (round-2 review's blast-radius concern). The representative handler errors
// classify the same as the pre-fix generic "workflow failed" — UNKNOWN — so
// no dashboard/recovery behaviour keyed on TaskFailureClass shifts. (If a
// future handler error string trips a classifier needle, that's a classifier
// bug to fix there, not a reason to hide the error from operators.)
func TestSystemStepOnFail_NoClassificationDrift(t *testing.T) {
	reg := NewSystemHandlerRegistry()
	reg.Register(&failingSystemHandler{name: "rag.extract", retErr: errors.New("extraction failed: corrupt PDF")})
	err := runSystemStepFail(t, reg)
	require.Error(t, err)
	assert.Equal(t, persistence.TaskFailureClassUnknown, ClassifyExecutionFailure(err, ""),
		"a representative handler error must not reclassify away from UNKNOWN (no drift vs the pre-fix generic terminal)")
}

// markerError is a distinct concrete error type for the errors.As chain check.
type markerError struct{ msg string }

func (e *markerError) Error() string { return e.msg }

// okSystemHandler succeeds, returning a small result envelope.
type okSystemHandler struct{ name string }

func (h *okSystemHandler) Name() string { return h.name }
func (h *okSystemHandler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	return SystemStepResult{Result: json.RawMessage(`{"ok":true}`)}, nil
}

// TestSystemStepOnFail_LaterSuccessClearsHandlerError is the §7 gate test:
// a system step fails via OnFail into a step that SUCCEEDS and the workflow
// then reaches a COMPLETED terminal — the (success) terminal must carry NO
// handler error. This holds because resolveTerminalOutcome's COMPLETED case
// never reads lastResultMessage (only the FAILED case does), so a lingering
// message from the earlier failed step cannot leak into a successful terminal.
func TestSystemStepOnFail_LaterSuccessClearsHandlerError(t *testing.T) {
	reg := NewSystemHandlerRegistry()
	reg.Register(&failingSystemHandler{name: "step.fail", retErr: errors.New("first-step boom")})
	reg.Register(&okSystemHandler{name: "step.ok"})

	rt := NewMockRuntime()
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s"},
		workflow: &registry.Workflow{
			ID:            "wf",
			Entrypoint:    "step1",
			MaxIterations: 10,
			MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				// step1 fails → recover (a SUCCEEDING system step) → done.
				"step1":   {Type: "system", Handler: "step.fail", OnFail: "recover"},
				"recover": {Type: "system", Handler: "step.ok", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"done": {Status: "COMPLETED"},
			},
		},
	}
	exec := &persistence.Execution{ID: "x-clear", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.NoError(t, err,
		"a failed system step whose OnFail recovers to success must COMPLETE with no error")
}
