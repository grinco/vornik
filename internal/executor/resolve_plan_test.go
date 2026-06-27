package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestResolveExecutionPlan_WithResolver_SwarmNotFound — project lands
// but its SwarmID doesn't resolve → error.
func TestResolveExecutionPlan_WithResolver_SwarmNotFound(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "missing-swarm"},
		},
	}
	e.SetWorkflowResolver(resolver)
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	_, err := e.resolveExecutionPlan(context.Background(), task, exec)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "swarm")
	assert.Contains(t, err.Error(), "not found")
}

// TestResolveExecutionPlan_WithResolver_WorkflowNotFound — project +
// swarm resolved but workflow ID isn't.
func TestResolveExecutionPlan_WithResolver_WorkflowNotFound(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "missing-workflow"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker"}}},
		},
	}
	e.SetWorkflowResolver(resolver)
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	_, err := e.resolveExecutionPlan(context.Background(), task, exec)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workflow")
	assert.Contains(t, err.Error(), "not found")
}

// TestResolveExecutionPlan_WithResolver_TaskWorkflowOverridesProjectDefault —
// when the task carries a non-empty WorkflowID, it wins over the
// project's DefaultWorkflowID.
func TestResolveExecutionPlan_WithResolver_TaskWorkflowOverridesProjectDefault(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "default-wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker"}}},
		},
		workflows: map[string]*registry.Workflow{
			"default-wf": {ID: "default-wf", Entrypoint: "x"},
			"custom-wf":  {ID: "custom-wf", Entrypoint: "y"},
		},
	}
	e.SetWorkflowResolver(resolver)
	custom := "custom-wf"
	task := &persistence.Task{ID: "t1", ProjectID: "p1", WorkflowID: &custom}
	exec := &persistence.Execution{ID: "e1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	assert.Equal(t, "custom-wf", plan.workflow.ID)
}

// TestResolveExecutionPlan_WithResolver_TaskWorkflowDashFallsBackToDefault —
// the "-" placeholder is treated as "unset" by taskWorkflowID's logic;
// the project's default workflow wins.
func TestResolveExecutionPlan_WithResolver_TaskWorkflowDashFallsBackToDefault(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "default-wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker"}}},
		},
		workflows: map[string]*registry.Workflow{
			"default-wf": {ID: "default-wf", Entrypoint: "x"},
		},
	}
	e.SetWorkflowResolver(resolver)
	dash := "-"
	task := &persistence.Task{ID: "t1", ProjectID: "p1", WorkflowID: &dash}
	exec := &persistence.Execution{ID: "e1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	assert.Equal(t, "default-wf", plan.workflow.ID)
}

// TestResolveExecutionPlan_WithResolver_AgentRoleMissingErrors — the
// validator rejects a workflow whose agent step references a role
// not declared in the swarm.
func TestResolveExecutionPlan_WithResolver_AgentRoleMissingErrors(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker"}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf": {
				ID: "wf",
				Steps: map[string]registry.WorkflowStep{
					"step1": {Type: "agent", Role: "missing-role"},
				},
			},
		},
	}
	e.SetWorkflowResolver(resolver)
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	_, err := e.resolveExecutionPlan(context.Background(), task, exec)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing-role")
	assert.Contains(t, err.Error(), "not present in swarm")
}

// TestWithWorkflowResolver_AssignsNonNilResolver — happy path the
// existing nil-resolver test misses.
func TestWithWorkflowResolver_AssignsNonNilResolver(t *testing.T) {
	e := &Executor{}
	resolver := &MockWorkflowResolver{}
	WithWorkflowResolver(resolver)(e)
	assert.Same(t, resolver, e.workflows)
}

// TestSetWorkflowResolver_AssignsNonNilResolver — symmetric runtime
// setter; the nil-receiver / nil-arg branches are covered elsewhere.
func TestSetWorkflowResolver_AssignsNonNilResolver(t *testing.T) {
	e := &Executor{}
	resolver := &MockWorkflowResolver{}
	e.SetWorkflowResolver(resolver)
	assert.Same(t, resolver, e.workflows)
}

// TestResolveExecutionPlan_WithResolver_PlanStepRoleRewrittenToLeadRole —
// for plan-type steps, an absent role gets rewritten to the swarm's
// configured LeadRole.
func TestResolveExecutionPlan_WithResolver_PlanStepRoleRewrittenToLeadRole(t *testing.T) {
	e, _, _, _, _ := setup()
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", LeadRole: "architect", Roles: []registry.SwarmRole{
				{Name: "architect"},
				{Name: "worker"},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf": {
				ID: "wf",
				Steps: map[string]registry.WorkflowStep{
					// Plan step references a generic "lead" role that's
					// not in the swarm — the validator rewrites it.
					"plan": {Type: "plan", Role: "lead"},
				},
			},
		},
	}
	e.SetWorkflowResolver(resolver)
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	plan, err := e.resolveExecutionPlan(context.Background(), task, exec)
	require.NoError(t, err)
	rewritten := plan.workflow.Steps["plan"]
	assert.Equal(t, "architect", rewritten.Role, "plan step's role must be rewritten to swarm.LeadRole")
}
