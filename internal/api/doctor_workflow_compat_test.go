package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/registry"
)

// TestMissingRolesForWorkflow is the shared helper used by both the
// workflow-compat and eval-lint checks. The function's contract is
// "roles the workflow's agent/plan steps require that the swarm
// doesn't offer" — sorted, deduped.
func TestMissingRolesForWorkflow(t *testing.T) {
	wf := &registry.Workflow{
		ID: "demo",
		Steps: map[string]registry.WorkflowStep{
			"plan":      {Type: "plan", Role: "lead"},
			"implement": {Type: "agent", Role: "coder"},
			"review":    {Type: "agent", Role: "reviewer"},
			"gate":      {Type: "gate"},
		},
	}
	// Swarm has lead + coder but no reviewer → only reviewer should
	// appear in the missing list.
	missing := missingRolesForWorkflow(wf, map[string]bool{"lead": true, "coder": true})
	assert.Equal(t, []string{"reviewer"}, missing)

	// Swarm with every role → no missing.
	missing = missingRolesForWorkflow(wf,
		map[string]bool{"lead": true, "coder": true, "reviewer": true})
	assert.Empty(t, missing)

	// Gate steps don't require a role — should not contribute to
	// missing even when none of the agent roles are present.
	gateOnly := &registry.Workflow{
		ID:    "gate-only",
		Steps: map[string]registry.WorkflowStep{"g": {Type: "gate"}},
	}
	assert.Empty(t, missingRolesForWorkflow(gateOnly, map[string]bool{}))
}
