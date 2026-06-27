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

// TestExecutePlanStep_ResumePath_AllRolesAlreadyDone — when
// state.PlanSteps is populated and PlanIndex >= len(PlanSteps),
// the loop body doesn't execute. The function finalizes the
// lead's pending row and returns OnSuccess with the existing
// completedSteps unchanged. Exercises the resume-after-restart
// path where the daemon was killed mid-plan but every role had
// already finished.
//
// The runLeadPlanning branch is bypassed because state.PlanSteps
// is non-empty.
func TestExecutePlanStep_ResumeAllDone(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-resume", ProjectID: "p", CreatedAt: time.Now()}
	tr.AddTask(task)

	exec := &persistence.Execution{ID: "x-resume", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "researcher"},
			{Name: "writer"},
		}},
		workflow:    &registry.Workflow{ID: "wf"},
		worktreeDir: "", // skips git HEAD checks
	}

	step := registry.WorkflowStep{
		Type:      "plan",
		Role:      "lead",
		OnSuccess: "final",
	}

	state := &executionState{
		PlanSteps:       []string{"researcher", "writer"},
		PlanIndex:       2, // past the end — loop is skipped
		PlanLeadStepID:  "plan_lead_lead",
		PlanLeadMessage: "do these things",
		PlanStartHEAD:   "", // skips claim checks
	}

	cid, result, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan_step", step, time.Minute, state,
		[]string{"prior_step"}, nil,
	)
	require.NoError(t, err)
	// No agent calls were made (resume past-end).
	assert.Equal(t, "", cid)
	assert.Nil(t, result)
	assert.Equal(t, "final", nextStep, "OnSuccess transition returned at end of plan")
	// completedSteps wasn't extended (no per-role IDs appended since
	// the loop didn't run).
	assert.Equal(t, []string{"prior_step"}, completedSteps)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, 0, rt.startCalls, "no container starts in pure-resume scenario")
}

// TestExecutePlanStep_ResumeFromIndex_AgentRunPath — with
// state.PlanSteps populated and PlanIndex < len(PlanSteps),
// the loop drives executeAgentStepWithFallback. With a
// MockRuntime that errors on StartContainer the loop must
// surface the role failure and attribute it back to the lead.
func TestExecutePlanStep_AgentFailureAttributedToLead(t *testing.T) {
	rt := NewMockRuntime()
	rt.startErr = assertErrf("container start failed")
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	outcomes := newStubStepOutcomeRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.outcomeRepo = outcomes
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-fail", ProjectID: "p", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-fail", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	// Seed a pending_validation outcome for the lead so the
	// downstream-rejected attribution path can flip it.
	e.recordStepOutcome(context.Background(), task, exec, "plan_lead_lead", "lead", "model",
		"pending_validation", "", "", nil, nil)

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{
		PlanSteps:      []string{"researcher"},
		PlanIndex:      0,
		PlanLeadStepID: "plan_lead_lead",
	}

	_, _, _, _, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan_step", step, time.Minute, state, nil, nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "researcher")
	assert.Contains(t, err.Error(), "failed")

	// The lead's pending row got finalized as downstream_rejected
	// with attribution to the failing role's synthetic step.
	require.NotEmpty(t, outcomes.rows)
	found := false
	for _, r := range outcomes.rows {
		if r.StepID == "plan_lead_lead" && r.Outcome == "downstream_rejected" {
			found = true
			require.NotNil(t, r.AttributedToStepID)
			assert.Contains(t, *r.AttributedToStepID, "researcher")
		}
	}
	assert.True(t, found, "lead's pending row must flip to downstream_rejected on child failure")
}

// TestExecutePlanStep_MultiRoleSuccess — drives a 2-role plan
// where both roles succeed and exercises the per-role
// previousResult chaining (role i's `message` becomes role i+1's
// PreviousResult). Verifies completedSteps gets one entry per
// role in order.
func TestExecutePlanStep_MultiRoleSuccess(t *testing.T) {
	rt := NewMockRuntime()
	// Sequence: two different result blobs across the two role
	// calls so we can confirm the chaining honoured order.
	rt.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"first role done"}`,
		`{"status":"COMPLETED","message":"second role done"}`,
	}
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-multi", ProjectID: "p", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-multi", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			{Name: "writer", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "final"}
	state := &executionState{
		PlanSteps: []string{"researcher", "writer"},
		PlanIndex: 0,
	}

	_, _, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan_step", step, time.Minute, state, nil, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "final", nextStep)
	require.Len(t, completedSteps, 2, "one synthetic step per role")
	assert.Contains(t, completedSteps[0], "researcher")
	assert.Contains(t, completedSteps[1], "writer")
	// PlanIndex must be advanced past the end so a future resume
	// skips the loop.
	assert.Equal(t, 2, state.PlanIndex)
}

// TestExecutePlanStep_HappyPathFinalizesLeadOK — the
// plan-quality attribution path: every child succeeds, the
// lead's pending row gets flipped to OK before return. Seeds a
// pending lead row and verifies the finalize landed.
func TestExecutePlanStep_HappyPathFinalizesLeadOK(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	outcomes := newStubStepOutcomeRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.outcomeRepo = outcomes
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-lead-ok", ProjectID: "p", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-lead-ok", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	// Seed pending_validation for the lead.
	e.recordStepOutcome(context.Background(), task, exec, "plan_lead_id",
		"lead", "model", "pending_validation", "", "", nil, nil)

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "writer", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{
		PlanSteps:      []string{"writer"},
		PlanIndex:      0,
		PlanLeadStepID: "plan_lead_id",
	}

	_, _, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan_step", step, time.Minute, state, nil, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "done", nextStep)
	require.Len(t, completedSteps, 1)

	// Lead's pending row was finalized OK after the writer succeeded.
	foundOK := false
	for _, r := range outcomes.rows {
		if r.StepID == "plan_lead_id" && r.Outcome == "ok" {
			foundOK = true
		}
	}
	assert.True(t, foundOK,
		"lead's pending row must flip to OK once every spawned child succeeded")
}

// TestExecutePlanStep_ResumeHappyPath — state.PlanSteps populated
// with one role, MockRuntime returns success. The loop runs the
// role, appends the synthetic step ID to completedSteps, and
// returns OnSuccess.
func TestExecutePlanStep_ResumeHappyPath(t *testing.T) {
	rt := NewMockRuntime()
	// MockRuntime's StartContainer writes the supplied outputJSON
	// into result.json under c.OutputDir. The agent's result must
	// parse cleanly as JSON for executeAgentStep's downstream
	// classifier — minimum viable shape.
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	task := &persistence.Task{ID: "t-happy", ProjectID: "p", CreatedAt: time.Now()}
	tr.AddTask(task)
	exec := &persistence.Execution{ID: "x-happy", TaskID: task.ID, ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))

	plan := &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "researcher", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{ID: "wf"},
	}
	step := registry.WorkflowStep{Type: "plan", Role: "lead", OnSuccess: "done"}
	state := &executionState{
		PlanSteps:      []string{"researcher"},
		PlanIndex:      0,
		PlanLeadStepID: "",
	}

	cid, _, nextStep, completedSteps, err := e.executePlanStep(
		context.Background(), task, exec, plan,
		"plan_step", step, time.Minute, state, nil, nil,
	)
	require.NoError(t, err, "successful agent run must not propagate an error")
	assert.NotEmpty(t, cid, "container id from the successful role must surface")
	assert.Equal(t, "done", nextStep)
	// One synthetic step ID was appended for the researcher.
	require.Len(t, completedSteps, 1)
	assert.Contains(t, completedSteps[0], "researcher",
		"per-role synthetic step ID must include the role name")
}
