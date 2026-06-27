package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestResolveExecutionPlan_NoResolver — without a workflow
// resolver, the executor falls back to a default single-step
// workflow + default-swarm. This is the test-deployment path
// where no registry is wired.
func TestResolveExecutionPlan_NoResolver(t *testing.T) {
	e := &Executor{
		config: &Config{RuntimeImage: "test-image:latest"},
		logger: zerolog.Nop(),
	}
	exec := &persistence.Execution{ID: "x"}
	plan, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		exec)
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "default-swarm", plan.swarm.ID)
	require.Len(t, plan.swarm.Roles, 1)
	assert.Equal(t, "worker", plan.swarm.Roles[0].Name)
	assert.Equal(t, "test-image:latest", plan.swarm.Roles[0].Runtime.Image)
	assert.Equal(t, "default-workflow", exec.WorkflowID,
		"execution.WorkflowID must be populated for downstream display")
	require.NotNil(t, plan.workflow)
	assert.Equal(t, "execute", plan.workflow.Entrypoint)
}

// TestResolveExecutionPlan_ProjectNotFound — resolver wired but
// the project ID doesn't exist in the catalogue. Surfaces a
// structured error so the operator sees "X not found" not
// silent skip.
func TestResolveExecutionPlan_ProjectNotFound(t *testing.T) {
	e := &Executor{
		config:    &Config{},
		workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{}},
		logger:    zerolog.Nop(),
	}
	_, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "missing"},
		&persistence.Execution{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project missing not found")
}

// TestResolveExecutionPlan_SwarmNotFound — project resolves but
// its SwarmID doesn't exist. Same actionable diagnostic for
// the operator.
func TestResolveExecutionPlan_SwarmNotFound(t *testing.T) {
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "missing-swarm", DefaultWorkflowID: "wf"},
			},
		},
		logger: zerolog.Nop(),
	}
	_, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "swarm missing-swarm not found")
}

// TestResolveExecutionPlan_WorkflowNotFound — project + swarm
// resolve but the workflow ID doesn't. Diagnostic includes both
// workflow and project IDs.
func TestResolveExecutionPlan_WorkflowNotFound(t *testing.T) {
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "missing-wf"},
			},
			swarms: map[string]*registry.Swarm{
				"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker"}}},
			},
		},
		logger: zerolog.Nop(),
	}
	_, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflow missing-wf not found")
}

// TestResolveExecutionPlan_RoleNotInSwarm — a workflow step
// references a role the swarm doesn't define. Surfaces with the
// step ID, role, swarm, and project so the operator can edit
// the right yaml file.
func TestResolveExecutionPlan_RoleNotInSwarm(t *testing.T) {
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "wf"},
			},
			swarms: map[string]*registry.Swarm{
				"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker"}}},
			},
			workflows: map[string]*registry.Workflow{
				"wf": {
					ID:         "wf",
					Entrypoint: "missing_role_step",
					Steps: map[string]registry.WorkflowStep{
						"missing_role_step": {Type: "agent", Role: "reviewer"},
					},
				},
			},
		},
		logger: zerolog.Nop(),
	}
	_, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_role_step")
	assert.Contains(t, err.Error(), "reviewer")
	assert.Contains(t, err.Error(), "swarm s")
}

// TestResolveExecutionPlan_PlanStepUsesLeadRoleSubstitution —
// when a workflow step is type=plan with a role the swarm
// doesn't have, but the swarm has LeadRole configured, the
// step's role gets rewritten to the swarm's lead instead of
// failing. Lets a shared "adaptive" workflow work across
// swarms with different lead-role names.
func TestResolveExecutionPlan_PlanStepLeadRoleSubstitution(t *testing.T) {
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "wf"},
			},
			swarms: map[string]*registry.Swarm{
				"s": {ID: "s", LeadRole: "coordinator", Roles: []registry.SwarmRole{
					{Name: "coordinator"}, {Name: "worker"},
				}},
			},
			workflows: map[string]*registry.Workflow{
				"wf": {
					ID:         "wf",
					Entrypoint: "lead_step",
					Steps: map[string]registry.WorkflowStep{
						"lead_step": {Type: "plan", Role: "lead"}, // swarm has no "lead"
					},
				},
			},
		},
		logger: zerolog.Nop(),
	}
	plan, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	// The plan step's role was rewritten to the swarm's LeadRole.
	assert.Equal(t, "coordinator", plan.workflow.Steps["lead_step"].Role,
		"plan step's role must be substituted to swarm.LeadRole")
}

// TestResolveExecutionPlan_TaskOverridesWorkflow — when the
// task carries a WorkflowID, the executor uses that instead of
// the project's DefaultWorkflowID. Used by the route-step
// workflow override path.
func TestResolveExecutionPlan_TaskOverridesWorkflow(t *testing.T) {
	override := "custom-wf"
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "default-wf"},
			},
			swarms: map[string]*registry.Swarm{
				"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker"}}},
			},
			workflows: map[string]*registry.Workflow{
				"default-wf": {ID: "default-wf", Entrypoint: "s1",
					Steps: map[string]registry.WorkflowStep{
						"s1": {Type: "agent", Role: "worker"},
					}},
				"custom-wf": {ID: "custom-wf", Entrypoint: "s2",
					Steps: map[string]registry.WorkflowStep{
						"s2": {Type: "agent", Role: "worker"},
					}},
			},
		},
		logger: zerolog.Nop(),
	}
	exec := &persistence.Execution{ID: "x"}
	plan, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p", WorkflowID: &override},
		exec)
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "custom-wf", plan.workflow.ID,
		"task.WorkflowID must override project.DefaultWorkflowID")
	assert.Equal(t, "custom-wf", exec.WorkflowID,
		"execution.WorkflowID must reflect the override")
}

// TestResolveExecutionPlan_DashWorkflowIDIgnored — the LLM
// placeholder "-" must NOT be treated as a real workflow id;
// the default is used instead. Pinned because models routinely
// emit "-" as an "I don't know" answer.
func TestResolveExecutionPlan_DashWorkflowIDIgnored(t *testing.T) {
	dash := "-"
	e := &Executor{
		config: &Config{},
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", SwarmID: "s", DefaultWorkflowID: "default-wf"},
			},
			swarms: map[string]*registry.Swarm{
				"s": {ID: "s", Roles: []registry.SwarmRole{{Name: "worker"}}},
			},
			workflows: map[string]*registry.Workflow{
				"default-wf": {ID: "default-wf", Entrypoint: "s1",
					Steps: map[string]registry.WorkflowStep{
						"s1": {Type: "agent", Role: "worker"},
					}},
			},
		},
		logger: zerolog.Nop(),
	}
	plan, err := e.resolveExecutionPlan(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p", WorkflowID: &dash},
		&persistence.Execution{ID: "x"})
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "default-wf", plan.workflow.ID,
		"task.WorkflowID='-' must fall back to project default")
}
