package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/verifier"
)

// TestDispatchAgentStep_SynthesisedPaths covers the two no-LLM dispatch
// branches extracted in Track-B Phase 3: a resume (routeAlreadyHandled) that
// surfaces the children's outcomes, and a single-candidate strict-route
// auto-route. The container-running path needs a live runtime and is covered
// by the agent-path integration tests.
func TestDispatchAgentStep_SynthesisedPaths(t *testing.T) {
	e := &Executor{} // execRepo nil → synthesizeResumeResult reports status only
	task := &persistence.Task{ID: "parent-1"}
	execution := &persistence.Execution{ID: "exec-1"}

	t.Run("resume surfaces child outcomes, no container", func(t *testing.T) {
		children := []*persistence.Task{
			{ID: "c1", Status: persistence.TaskStatusCompleted},
			{ID: "c2", Status: persistence.TaskStatusCompleted},
		}
		cid, res, err := e.dispatchAgentStep(context.Background(), task, execution,
			&executionPlan{}, "route", registry.WorkflowStep{}, 0, &agentInputOpts{}, nil, true, children)
		if err != nil {
			t.Fatalf("resume path must not error, got %v", err)
		}
		if cid != "" {
			t.Fatalf("resume path runs no container, want empty cid, got %q", cid)
		}
		// Carries neither selected_workflow nor delegatedTasks (so the caller's
		// spawn branches short-circuit on resume).
		if strings.Contains(string(res), "selected_workflow") || strings.Contains(string(res), "delegatedTasks") {
			t.Fatalf("resume result must not carry spawn fields, got %s", res)
		}
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(res, &parsed) != nil || parsed.Message == "" {
			t.Fatalf("resume result must carry a non-empty message, got %s", res)
		}
	})

	t.Run("single-candidate auto-routes without LLM", func(t *testing.T) {
		plan := &executionPlan{
			workflow: &registry.Workflow{ID: "adaptive", Entrypoint: "route"},
			project:  &registry.Project{AdaptiveCandidateWorkflows: []string{"research"}},
		}
		cid, res, err := e.dispatchAgentStep(context.Background(), task, execution,
			plan, "route", registry.WorkflowStep{}, 0, &agentInputOpts{}, nil, false, nil)
		if err != nil {
			t.Fatalf("single-candidate path must not error, got %v", err)
		}
		if cid != "" {
			t.Fatalf("auto-route runs no container, want empty cid, got %q", cid)
		}
		var parsed struct {
			SelectedWorkflow string `json:"selected_workflow"`
		}
		if json.Unmarshal(res, &parsed) != nil {
			t.Fatalf("auto-route result must be JSON, got %s", res)
		}
		if parsed.SelectedWorkflow != "research" {
			t.Fatalf("auto-route must select the only candidate, got %q", parsed.SelectedWorkflow)
		}
	})
}

// TestBuildStepFailureRecovery pins the failure-class mapping extracted in
// Track-B Phase 3: pedantic mode opts out (nil), a verifier block carries its
// BlockedURLs, and any other error collapses through the canonical classifier
// to a non-empty lead-facing class.
func TestBuildStepFailureRecovery(t *testing.T) {
	plan := &executionPlan{workflow: &registry.Workflow{ID: "wf"}, project: &registry.Project{}}

	t.Run("pedantic mode opts out", func(t *testing.T) {
		task := &persistence.Task{Payload: []byte(`{"context":{"pedantic":true}}`)}
		if rc := buildStepFailureRecovery(errors.New("boom"), "step1", task, plan); rc != nil {
			t.Fatalf("pedantic mode must return nil recovery context, got %+v", rc)
		}
	})

	t.Run("verifier block carries blocked urls", func(t *testing.T) {
		task := &persistence.Task{}
		rve := &RecoverableVerifierError{
			Err:         errors.New("portal blocked"),
			BlockedURLs: []verifier.BlockedURL{{URL: "https://x.test", Reason: "http_403"}},
		}
		rc := buildStepFailureRecovery(rve, "step1", task, plan)
		if rc == nil {
			t.Fatal("verifier error must produce a recovery context")
		}
		if rc.FailureClass != "verifier_block" {
			t.Fatalf("want verifier_block, got %q", rc.FailureClass)
		}
		if len(rc.BlockedURLs) != 1 || rc.BlockedURLs[0].URL != "https://x.test" {
			t.Fatalf("blocked urls must be forwarded, got %+v", rc.BlockedURLs)
		}
		if rc.FailedStep != "step1" {
			t.Fatalf("failed step must be recorded, got %q", rc.FailedStep)
		}
	})

	t.Run("generic error maps through the classifier", func(t *testing.T) {
		task := &persistence.Task{}
		rc := buildStepFailureRecovery(errors.New("some opaque failure"), "step1", task, plan)
		if rc == nil {
			t.Fatal("generic error must produce a recovery context")
		}
		if rc.FailureClass == "" {
			t.Fatal("generic error must map to a non-empty lead-facing class")
		}
		if rc.FailureReason == "" {
			t.Fatal("failure reason must be captured")
		}
	})
}
