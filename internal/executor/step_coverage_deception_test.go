package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestStepCoverage_DeceptionDetectionFiresEndToEnd is the first
// integration-style test for stability item 4 (executor step-coverage
// Tier 2). It exercises a full agent-step lifecycle through the
// executor — task lookup → workflow resolve → MockRuntime starts +
// produces result.json → result-claim verification → terminal status
// — and asserts that an agent fabricating testing.passed:true without
// actually running anything lands the task in FAILED with the right
// error class.
//
// This is the value Tier 2 is supposed to deliver: a real call
// through Execute() with stubbed runtime + repos, asserting DB state
// transitions. Without this kind of test, the deception-detection
// wiring from stability item 1 would only have unit coverage of the
// helpers — a refactor that drops the verifyRoleClaims call from
// container.go's executeAgentStep would silently break in production
// and leave only narrower tests still passing.
//
// Pattern for future Tier 2 tests: copy this scaffolding, change the
// outputJSON the MockRuntime produces, change the role's
// outputSchema, assert the new outcome. Each step type
// (agent / plan) and each failure class can be covered the same way.
func TestStepCoverage_DeceptionDetectionFiresEndToEnd(t *testing.T) {
	rt := NewMockRuntime()
	// Agent emits structurally-valid result.json claiming tests
	// passed but the toolAudit shows it never invoked an execution
	// tool. verifyRoleClaims must catch this and fail the step.
	rt.outputJSON = `{
		"testing": {"passed": true, "summary": "all green"},
		"toolAudit": [{"tool": "file_read"}, {"tool": "grep"}]
	}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:    "tester",
				Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
				// Use the same outputSchema shape the migrated tester
				// roles ship today, so the test exercises the actual
				// production schema path — derived RequiredOutputKeys
				// + PlausibilityRules flow through Validate.
				OutputSchema: &registry.OutputSchema{
					Type:     "object",
					Required: []string{"testing"},
					Properties: map[string]*registry.OutputSchema{
						"testing": {
							Type:     "object",
							Required: []string{"passed"},
							Properties: map[string]*registry.OutputSchema{
								"passed":   {Type: "bool"},
								"summary":  {Type: "string"},
								"failures": {Type: "string"},
							},
						},
					},
				},
				InjectSchemaIntoPrompt: true,
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "test",
				Steps: map[string]registry.WorkflowStep{
					"test": {Type: "agent", Role: "tester", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	})

	tr.AddTask(&persistence.Task{
		ID:          "t1",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1, // single-attempt so we land terminal quickly
		Payload:     []byte(`{"taskType":"test","context":{"prompt":"run the tests"}}`),
		CreatedAt:   time.Now(),
	})

	assert.NoError(t, e.Execute("t1"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t1")
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond, "task must finalize FAILED when deception detected")

	// The error must name the bypass — operators searching the failed
	// task's UI for "why" need a concrete pointer at the deception, not
	// a generic schema-violation that points anywhere.
	task, _ := tr.Get(context.Background(), "t1")
	if task.LastError == nil {
		t.Fatal("LastError unset on terminal-FAILED task")
	}
	if !strings.Contains(*task.LastError, "testing.passed:true") {
		t.Errorf("LastError missing deception detail; got: %q", *task.LastError)
	}
}

// TestStepCoverage_DeceptionDetectionAllowsHonestPass verifies the
// inverse: an agent that DID invoke an execution tool (run_shell etc.)
// AND emits passed:true is allowed through.
//
// Same harness pattern as the bypass test; only the outputJSON
// changes. This is the "future Tier 2 tests multiply from here"
// claim made concrete — adding a new test against this harness is a
// 5-line edit, not a rewrite.
func TestStepCoverage_DeceptionDetectionAllowsHonestPass(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{
		"testing": {"passed": true, "summary": "all green"},
		"toolAudit": [{"tool": "run_shell"}]
	}`
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, ar, tr, nil)
	e.config.RetryDelay = 0

	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{
				Name:    "tester",
				Runtime: registry.SwarmRoleRuntime{Image: "fake-agent:latest"},
				OutputSchema: &registry.OutputSchema{
					Type:     "object",
					Required: []string{"testing"},
					Properties: map[string]*registry.OutputSchema{
						"testing": {
							Type:     "object",
							Required: []string{"passed"},
							Properties: map[string]*registry.OutputSchema{
								"passed": {Type: "bool"},
							},
						},
					},
				},
				InjectSchemaIntoPrompt: true,
			}}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "test",
				Steps: map[string]registry.WorkflowStep{
					"test": {Type: "agent", Role: "tester", OnSuccess: "done"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"},
				},
			},
		},
	})

	tr.AddTask(&persistence.Task{
		ID:          "t2",
		ProjectID:   "p1",
		Status:      persistence.TaskStatusLeased,
		Attempt:     1,
		MaxAttempts: 1,
		Payload:     []byte(`{"taskType":"test","context":{"prompt":"run the tests"}}`),
		CreatedAt:   time.Now(),
	})

	assert.NoError(t, e.Execute("t2"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t2")
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond, "honest pass must finalize COMPLETED, not get tripped by the deception detector")
}
