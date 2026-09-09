package executor

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// `on_outcome` at EXECUTOR level (design
// 2026-09-01-forge-rereview-triggers-design.md §17.5).
//
// The assertion these make is "no reviewer ran", not "no review was posted".
// §16's guard already prevents the post, so a test written against the posted
// review would pass with or without this feature and prove nothing — the whole
// point here is the model spend, which is only visible as a container that was
// never started.

// runOutcomeWorkflow drives `system → agent` where the system step carries an
// on_outcome map, and reports which step the workflow finished on and whether
// the agent ever started.
// errFixedHandler is the failure the handler double returns.
var errFixedHandler = errors.New("the forge said no")

func runOutcomeWorkflow(t *testing.T, out json.RawMessage, handlerErr error,
	onOutcome map[string]string) (agentStarted bool, status string) {
	t.Helper()

	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	rt := &capturingRuntime{MockRuntime: mock}

	reg := NewSystemHandlerRegistry()
	reg.Register(&fixedSystemHandler{name: "test.fetch", out: out, err: handlerErr})

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
				Entrypoint: "fetch",
				Steps: map[string]registry.WorkflowStep{
					"fetch": {
						Type: "system", Handler: "test.fetch",
						OnSuccess: "review", OnFail: "failed",
						OnOutcome: onOutcome,
					},
					"review": {
						Type: "agent", Role: "reviewer",
						Prompt:    "Review the change described by the previous step.",
						OnSuccess: "done", OnFail: "failed",
					},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done":              {Status: "COMPLETED"},
					"nothing_to_review": {Status: "COMPLETED"},
					"failed":            {Status: "FAILED"},
				},
			},
		},
	})

	const taskID = "t-outcome"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"review the change"}}`),
		CreatedAt: time.Now(),
	})
	_ = e.Execute(taskID)

	// The agent step, if it runs at all, starts its container synchronously
	// within Execute — so by here the capture either happened or never will.
	// A short settle for the same reason the sibling suite polls.
	for i := 0; i < 40 && rt.latestCapture() == nil; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	task, err := tr.Get(t.Context(), taskID)
	require.NoError(t, err)
	return rt.latestCapture() != nil, string(task.Status)
}

// THE CASE THIS FEATURE EXISTS FOR: a no-change fetch reaches a terminal and
// the reviewer never starts.
//
// Fails against code without on_outcome routing — there the workflow takes
// on_success into the agent step, pays for a review of nothing, and §16's guard
// then refuses to post it.
func TestOnOutcome_NoChangeSkipsTheReviewerEntirely(t *testing.T) {
	started, status := runOutcomeWorkflow(t,
		json.RawMessage(`{"message":"No new commits since the last review of abc123.","scope":"no-change","outcome":"no-change"}`),
		nil,
		map[string]string{"no-change": "nothing_to_review"})

	if started {
		t.Error("the reviewer ran on a no-change diff — the model spend this routes around was paid anyway")
	}
	if status != string(persistence.TaskStatusCompleted) {
		t.Errorf("task status = %q, want COMPLETED — routing to a terminal is a normal finish, not a failure", status)
	}
}

// An outcome the map does not name still runs the reviewer. Pinned because the
// dangerous failure of this feature is the opposite one: a review silently not
// happening on a change that HAS commits.
func TestOnOutcome_AnIncrementalDiffStillReviews(t *testing.T) {
	started, _ := runOutcomeWorkflow(t,
		json.RawMessage(`{"message":"diff --git a/x b/x","scope":"incremental","outcome":"incremental"}`),
		nil,
		map[string]string{"no-change": "nothing_to_review"})

	if !started {
		t.Error("an incremental diff must still be reviewed; on_outcome named no route for it")
	}
}

// A FAILING handler takes on_fail even when on_outcome names its outcome. A
// failure is not an outcome, and the reverse would turn an error into a silent
// branch that looks like a successful decision.
func TestOnOutcome_AFailureStillTakesOnFail(t *testing.T) {
	started, status := runOutcomeWorkflow(t,
		json.RawMessage(`{"outcome":"no-change"}`),
		errFixedHandler,
		map[string]string{"no-change": "nothing_to_review"})

	if started {
		t.Error("a failed system step must not start the agent")
	}
	if status != string(persistence.TaskStatusFailed) {
		t.Errorf("task status = %q, want FAILED — on_outcome must not swallow an error", status)
	}
}
