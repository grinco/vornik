package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// A CHAIN of system steps before an agent (design
// 2026-09-03-system-step-output-reaches-the-next-agent-design.md, amendment
// 2026-09-09).
//
// INCIDENT, headmatch PR #53: `github-review` gained a second system step, so
// the chain became fetch_diff → fetch_ci → review. `lastResultMessage` was one
// variable, fetch_ci overwrote the diff, and the reviewer — whose prompt says
// the diff "was provided as your input" — improvised, described a change from a
// DIFFERENT pull request, and posted an approval.
//
// At executor level for the same reason the original tests are: both handlers
// were correct and the agent was correct. The defect lives in the seam.

// runSystemChainThenAgent drives `system → system → agent` and returns the
// context map from the task.json the agent actually received.
func runSystemChainThenAgent(t *testing.T, first, second SystemHandler) map[string]any {
	t.Helper()

	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	rt := &capturingRuntime{MockRuntime: mock}

	reg := NewSystemHandlerRegistry()
	reg.Register(first)
	reg.Register(second)

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
				Entrypoint: "fetch_diff",
				Steps: map[string]registry.WorkflowStep{
					"fetch_diff": {
						Type: "system", Handler: first.Name(),
						OnSuccess: "fetch_ci", OnFail: "failed",
					},
					"fetch_ci": {
						Type: "system", Handler: second.Name(),
						OnSuccess: "review", OnFail: "review",
					},
					"review": {
						Type: "agent", Role: "reviewer",
						Prompt:    "Review the change described by the previous step.",
						OnSuccess: "done", OnFail: "failed",
					},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	const taskID = "t-syschain"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"review the change"}}`),
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
	require.NotNil(t, raw, "the agent step must have started and been handed a task.json")

	var payload struct {
		Context map[string]any `json:"context"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	return payload.Context
}

// THE REGRESSION. Both handlers' output reaches the agent.
//
// Fails against pre-amendment code, which carried only the second: the diff was
// gone and the agent held the CI summary alone.
func TestSystemChain_EveryHandlersOutputReachesTheAgent(t *testing.T) {
	const diff = "diff --git a/headmatch/exporters.py b/headmatch/exporters.py"
	const ci = "CI outcomes for this commit (5 run(s)):"

	ctxMap := runSystemChainThenAgent(t,
		&fixedSystemHandler{name: "forge.fetch_diff", out: json.RawMessage(
			`{"message":"` + diff + `","scope":"full"}`)},
		&fixedSystemHandler{name: "forge.fetch_ci", out: json.RawMessage(
			`{"message":"` + ci + `","failed":1}`)},
	)

	got, _ := ctxMap["previousStepResult"].(string)
	if !strings.Contains(got, diff) {
		t.Errorf("THE DIFF IS MISSING — this is headmatch PR #53, where the reviewer "+
			"then invented a review of a different pull request:\n%s", got)
	}
	if !strings.Contains(got, ci) {
		t.Errorf("the CI context is missing:\n%s", got)
	}
	// Ordering is execution order, so the agent reads the diff before its
	// enrichment rather than after it.
	if strings.Index(got, diff) > strings.Index(got, ci) {
		t.Errorf("sections are out of execution order:\n%s", got)
	}
	// Labelled, because two payloads run together are ambiguous.
	for _, want := range []string{"fetch_diff", "forge.fetch_diff", "fetch_ci", "forge.fetch_ci"} {
		if !strings.Contains(got, want) {
			t.Errorf("section header missing %q:\n%s", want, got)
		}
	}
}

// A handler that returns no `message` adds nothing and does not WIPE what came
// before. Pre-amendment the empty branch cleared the carried message, so a
// fetch_ci with nothing to say would have deleted the diff.
func TestSystemChain_AnEmptyResultDoesNotWipeTheChain(t *testing.T) {
	const diff = "diff --git a/x b/x"
	ctxMap := runSystemChainThenAgent(t,
		&fixedSystemHandler{name: "forge.fetch_diff", out: json.RawMessage(`{"message":"` + diff + `"}`)},
		&fixedSystemHandler{name: "forge.fetch_ci", out: json.RawMessage(`{"detail":"no runs"}`)},
	)
	got, _ := ctxMap["previousStepResult"].(string)
	if !strings.Contains(got, diff) {
		t.Errorf("a silent second handler erased the first handler's output:\n%s", got)
	}
	// One real section, so no header: it must be byte-identical to the bare body.
	if got != diff {
		t.Errorf("a single contributing section must render bare, got:\n%s", got)
	}
}
