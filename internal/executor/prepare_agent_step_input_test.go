package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestAssembleStepPrompt pins the prompt-assembly behaviour extracted from
// executeWorkflowAttempt's agent case (Track-B Phase 1). The composition
// order is: base prompt → gate suffix → hint prefix → fork override prefix,
// and the fork override is one-shot (flips forkOverrideApplied so re-entries
// run with the unmodified-by-fork prompt).
func TestAssembleStepPrompt(t *testing.T) {
	e := &Executor{}

	t.Run("base prompt only", func(t *testing.T) {
		applied := false
		got := e.assembleStepPrompt(&persistence.Execution{}, "step1",
			registry.WorkflowStep{Prompt: "do the thing"}, "", &applied)
		if got != "do the thing" {
			t.Fatalf("want unchanged base prompt, got %q", got)
		}
		if applied {
			t.Fatal("forkOverrideApplied must stay false with no fork configured")
		}
	})

	t.Run("gate suffix appended after base", func(t *testing.T) {
		applied := false
		step := registry.WorkflowStep{
			Prompt: "review it",
			Gates:  []registry.WorkflowGate{{Condition: "review.approved == true", Target: "done"}},
		}
		got := e.assembleStepPrompt(&persistence.Execution{}, "step1", step, "", &applied)
		if !strings.HasPrefix(got, "review it") {
			t.Fatalf("gate suffix must follow the base prompt, got %q", got)
		}
		if !strings.Contains(got, "review.approved == true") {
			t.Fatalf("gate condition must appear in the suffix, got %q", got)
		}
	})

	t.Run("hint prefix prepended", func(t *testing.T) {
		applied := false
		got := e.assembleStepPrompt(&persistence.Execution{}, "step1",
			registry.WorkflowStep{Prompt: "base"}, "<hint>x</hint>\n", &applied)
		if got != "<hint>x</hint>\nbase" {
			t.Fatalf("hint must be prepended to the base prompt, got %q", got)
		}
	})

	t.Run("fork override prepended on first visit then flips flag", func(t *testing.T) {
		applied := false
		forkedStep := "step1"
		override := "OPERATOR GUIDANCE"
		exec := &persistence.Execution{
			ForkedFromStepID:     &forkedStep,
			ForkedPromptOverride: &override,
		}
		got := e.assembleStepPrompt(exec, "step1",
			registry.WorkflowStep{Prompt: "base"}, "", &applied)
		want := "OPERATOR GUIDANCE\n\n---\n\nbase"
		if got != want {
			t.Fatalf("fork override prepend: want %q, got %q", want, got)
		}
		if !applied {
			t.Fatal("forkOverrideApplied must flip true after applying the override")
		}

		// Second visit (flag already true) must NOT prepend again.
		again := e.assembleStepPrompt(exec, "step1",
			registry.WorkflowStep{Prompt: "base"}, "", &applied)
		if again != "base" {
			t.Fatalf("fork override is one-shot; re-entry must use the base prompt, got %q", again)
		}
	})

	t.Run("fork override skipped when step id does not match", func(t *testing.T) {
		applied := false
		other := "some-other-step"
		override := "OPERATOR GUIDANCE"
		exec := &persistence.Execution{
			ForkedFromStepID:     &other,
			ForkedPromptOverride: &override,
		}
		got := e.assembleStepPrompt(exec, "step1",
			registry.WorkflowStep{Prompt: "base"}, "", &applied)
		if got != "base" {
			t.Fatalf("override must not apply on a non-forked step, got %q", got)
		}
		if applied {
			t.Fatal("forkOverrideApplied must stay false when the step id does not match")
		}
	})
}

// TestResolveRoleOpts pins the role-resolution behaviour extracted from the
// agent case (Track-B Phase 1): a matching swarm role fills the role-specific
// opts fields and returns its config; a nil swarm or unmatched role returns
// nil and leaves opts untouched.
func TestResolveRoleOpts(t *testing.T) {
	e := &Executor{}
	task := &persistence.Task{}

	t.Run("nil swarm returns nil and leaves opts untouched", func(t *testing.T) {
		opts := &agentInputOpts{}
		got := e.resolveRoleOpts(&executionPlan{}, registry.WorkflowStep{Role: "writer"}, task, opts)
		if got != nil {
			t.Fatalf("nil swarm must return nil role config, got %+v", got)
		}
		if opts.SystemPrompt != "" || opts.Permissions != nil {
			t.Fatal("opts must be untouched when there is no swarm")
		}
	})

	t.Run("matching role fills opts and returns config", func(t *testing.T) {
		plan := &executionPlan{swarm: &registry.Swarm{
			Roles: []registry.SwarmRole{
				{Name: "writer", SystemPrompt: "WRITE WELL", ResponseFormat: "json_object"},
			},
		}}
		opts := &agentInputOpts{}
		got := e.resolveRoleOpts(plan, registry.WorkflowStep{Role: "writer"}, task, opts)
		if got == nil {
			t.Fatal("matching role must return its config")
		}
		if got.Name != "writer" {
			t.Fatalf("returned role config must be the matched role, got %q", got.Name)
		}
		// SystemPrompt goes through BuildEffectiveRolePrompt (prelude + role body).
		if !strings.Contains(opts.SystemPrompt, "WRITE WELL") {
			t.Fatalf("role system prompt must be composed into opts, got %q", opts.SystemPrompt)
		}
		if opts.Permissions == nil {
			t.Fatal("role permissions must be set on opts")
		}
		if opts.ResponseFormat != "json_object" {
			t.Fatalf("response format must be resolved, got %q", opts.ResponseFormat)
		}
	})

	t.Run("unmatched role returns nil", func(t *testing.T) {
		plan := &executionPlan{swarm: &registry.Swarm{
			Roles: []registry.SwarmRole{{Name: "writer"}},
		}}
		opts := &agentInputOpts{}
		got := e.resolveRoleOpts(plan, registry.WorkflowStep{Role: "reviewer"}, task, opts)
		if got != nil {
			t.Fatalf("unmatched role must return nil, got %+v", got)
		}
		if opts.SystemPrompt != "" {
			t.Fatal("opts must be untouched when no role matches")
		}
	})
}
