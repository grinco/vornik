package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestUI_SideEffectingUpstreamSteps — the UI retry-from-step containment
// guard mirrors the executor's: preserved-upstream steps of type
// system/call_project are flagged. Survivor computation follows the rewind
// (everything before the chosen step; failed-step path keeps all completed).
func TestUI_SideEffectingUpstreamSteps(t *testing.T) {
	reg := registry.New()
	require.NoError(t, reg.RegisterTransient("wf1", &registry.Workflow{
		Steps: map[string]registry.WorkflowStep{
			"plan":      {Type: "plan"},
			"implement": {Type: "agent"},
			"index":     {Type: "system", Handler: "rag.index"},
			"callfoo":   {Type: "call_project", TargetProject: "foo"},
			"review":    {Type: "agent"},
		},
	}))
	s := NewServer(WithProjectRegistry(reg))

	exec := &persistence.Execution{
		WorkflowID:     "wf1",
		CompletedSteps: []string{"plan", "index", "implement", "callfoo"},
	}

	// Retry from "callfoo": survivors are plan/index/implement → index flagged.
	assert.Equal(t, []string{"index"}, s.sideEffectingUpstreamSteps(exec, "callfoo"))

	// Retry from "index": survivors are just plan → nothing flagged.
	assert.Nil(t, s.sideEffectingUpstreamSteps(exec, "index"))

	// Failed-step path: chosen step not in completed_steps → all completed are
	// survivors, so both side-effecting steps are flagged.
	assert.Equal(t, []string{"index", "callfoo"}, s.sideEffectingUpstreamSteps(exec, "review"))

	// No registry wired → nil (best-effort, never blocks the rewind).
	bare := NewServer()
	assert.Nil(t, bare.sideEffectingUpstreamSteps(exec, "callfoo"))
}
