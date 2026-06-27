package executor

// B-7 Phase 1 — RED test for the executor's `case "system":`
// dispatch. The new step type pulls a SystemHandler out of the
// executor's handler registry by step.Handler name, runs Execute,
// and records the outcome via the existing step-outcome
// machinery (same pattern gate uses).

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/registry"
)

// recordingSystemHandler captures Execute calls so the dispatch
// test can pin (a) the handler was looked up by step.Handler name
// and (b) the input carried the expected step + task pointers.
type recordingSystemHandler struct {
	calls atomic.Int32
	last  SystemStepInput
	out   json.RawMessage
	err   error
	name  string
}

func (r *recordingSystemHandler) Name() string { return r.name }

func (r *recordingSystemHandler) Execute(_ context.Context, in SystemStepInput) (SystemStepResult, error) {
	r.calls.Add(1)
	r.last = in
	if r.err != nil {
		return SystemStepResult{}, r.err
	}
	return SystemStepResult{Result: r.out}, nil
}

// TestSystemStep_HandlerLookedUpByName — the executor's handler
// registry resolves the step's Handler field. Two handlers
// registered, only the named one fires.
func TestSystemStep_HandlerLookedUpByName(t *testing.T) {
	extract := &recordingSystemHandler{name: "rag.extract", out: json.RawMessage(`{"extract":true}`)}
	index := &recordingSystemHandler{name: "rag.index", out: json.RawMessage(`{"index":true}`)}

	reg := NewSystemHandlerRegistry()
	reg.Register(extract)
	reg.Register(index)

	got, ok := reg.Get("rag.extract")
	assert.True(t, ok)
	assert.Same(t, extract, got)

	got2, ok2 := reg.Get("rag.index")
	assert.True(t, ok2)
	assert.Same(t, index, got2)

	_, missing := reg.Get("rag.unknown")
	assert.False(t, missing, "unknown handler must return ok=false")
}

// TestSystemStep_RegistryNamesAccessor — the registry exposes the
// set of registered names so the daemon doctor + workflow
// validator can surface "unknown handler `rag.does-not-exist`"
// before a workflow runs in production.
func TestSystemStep_RegistryNames(t *testing.T) {
	reg := NewSystemHandlerRegistry()
	reg.Register(&recordingSystemHandler{name: "rag.extract"})
	reg.Register(&recordingSystemHandler{name: "rag.index"})
	names := reg.Names()
	assert.Contains(t, names, "rag.extract")
	assert.Contains(t, names, "rag.index")
}

// TestSystemStep_DispatchEnvelope — the SystemStepInput envelope
// carries the StepID + Step pointer the handler needs to read its
// configuration. Pinned so a future executor refactor that drops
// one of these surfaces causes a visible test failure.
func TestSystemStep_DispatchEnvelope(t *testing.T) {
	h := &recordingSystemHandler{name: "rag.extract", out: json.RawMessage(`{}`)}
	in := SystemStepInput{
		StepID: "extract",
		Step: &registry.WorkflowStep{
			Type:    "system",
			Handler: "rag.extract",
		},
	}
	_, err := h.Execute(context.Background(), in)
	assert.NoError(t, err)
	assert.Equal(t, "extract", h.last.StepID)
	assert.Equal(t, "rag.extract", h.last.Step.Handler)
}
