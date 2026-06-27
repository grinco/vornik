package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestEffectiveRoleModelForTask_NoOverride — non-counterfactual
// task (empty payload OR no counterfactual block) sees the same
// resolution as effectiveRoleModel. No behaviour change for
// existing deployments.
func TestEffectiveRoleModelForTask_NoOverride(t *testing.T) {
	e := &Executor{}
	role := &registry.SwarmRole{Name: "lead", Model: "claude-sonnet-4-6"}
	cases := []struct {
		name    string
		task    *persistence.Task
		wantOut string
	}{
		{"nil task", nil, "claude-sonnet-4-6"},
		{"empty payload", &persistence.Task{}, "claude-sonnet-4-6"},
		{"non-counterfactual payload", &persistence.Task{
			Payload: json.RawMessage(`{"context":{"prompt":"hi"}}`),
		}, "claude-sonnet-4-6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.effectiveRoleModelForTask(c.task, role)
			if got != c.wantOut {
				t.Errorf("got %q, want %q", got, c.wantOut)
			}
		})
	}
}

// TestEffectiveRoleModelForTask_RouterLevelOverride — payload
// carries model_override_all_roles; role's native model is
// overridden for every role.
func TestEffectiveRoleModelForTask_RouterLevelOverride(t *testing.T) {
	e := &Executor{}
	role := &registry.SwarmRole{Name: "researcher", Model: "claude-sonnet-4-6"}
	task := &persistence.Task{
		Payload: json.RawMessage(`{"context":{"counterfactual":{"model_override_all_roles":"claude-opus-4"}}}`),
	}
	got := e.effectiveRoleModelForTask(task, role)
	if got != "claude-opus-4" {
		t.Errorf("router-level override should win: got %q, want claude-opus-4", got)
	}
}

// TestEffectiveRoleModelForTask_PerRoleOverride — payload carries
// role_model_override.researcher; the role's native model is
// overridden for that role but NOT for other roles.
func TestEffectiveRoleModelForTask_PerRoleOverride(t *testing.T) {
	e := &Executor{}
	researcher := &registry.SwarmRole{Name: "researcher", Model: "claude-sonnet-4-6"}
	lead := &registry.SwarmRole{Name: "lead", Model: "claude-sonnet-4-6"}
	task := &persistence.Task{
		Payload: json.RawMessage(`{"context":{"counterfactual":{"role_model_override":{"researcher":"gpt-4o"}}}}`),
	}
	if got := e.effectiveRoleModelForTask(task, researcher); got != "gpt-4o" {
		t.Errorf("per-role override should apply to researcher: got %q", got)
	}
	if got := e.effectiveRoleModelForTask(task, lead); got != "claude-sonnet-4-6" {
		t.Errorf("other role should fall back: got %q", got)
	}
}

// TestEffectiveRoleModelForTask_PerRoleBeatsRouterLevel — both
// fields set; per-role wins for the matching role; router-level
// covers the rest.
func TestEffectiveRoleModelForTask_PerRoleBeatsRouterLevel(t *testing.T) {
	e := &Executor{}
	researcher := &registry.SwarmRole{Name: "researcher", Model: "claude-sonnet-4-6"}
	lead := &registry.SwarmRole{Name: "lead", Model: "claude-sonnet-4-6"}
	task := &persistence.Task{
		Payload: json.RawMessage(`{"context":{"counterfactual":{
			"model_override_all_roles": "haiku-4-5",
			"role_model_override": {"researcher": "gpt-4o"}
		}}}`),
	}
	if got := e.effectiveRoleModelForTask(task, researcher); got != "gpt-4o" {
		t.Errorf("per-role beats router-level for matching role: %q", got)
	}
	if got := e.effectiveRoleModelForTask(task, lead); got != "haiku-4-5" {
		t.Errorf("router-level covers non-matching roles: %q", got)
	}
}

// TestApplyCounterfactualPromptOverride_NoOverride — non-counterfactual
// task leaves opts.SystemPrompt untouched.
func TestApplyCounterfactualPromptOverride_NoOverride(t *testing.T) {
	opts := &agentInputOpts{SystemPrompt: "original system prompt"}
	applyCounterfactualPromptOverride(opts, &persistence.Task{}, "lead")
	if opts.SystemPrompt != "original system prompt" {
		t.Errorf("non-counterfactual task should leave prompt untouched: %q", opts.SystemPrompt)
	}
}

// TestApplyCounterfactualPromptOverride_HappyPath — payload carries
// the matching role; opts.SystemPrompt is replaced.
func TestApplyCounterfactualPromptOverride_HappyPath(t *testing.T) {
	opts := &agentInputOpts{SystemPrompt: "original system prompt"}
	task := &persistence.Task{
		Payload: json.RawMessage(`{"context":{"counterfactual":{"role_prompt_override":{"lead":"counterfactual prompt"}}}}`),
	}
	applyCounterfactualPromptOverride(opts, task, "lead")
	if opts.SystemPrompt != "counterfactual prompt" {
		t.Errorf("override should win: %q", opts.SystemPrompt)
	}
}

// TestApplyCounterfactualPromptOverride_RoleMismatch — payload
// overrides a different role; the call site's role is untouched.
func TestApplyCounterfactualPromptOverride_RoleMismatch(t *testing.T) {
	opts := &agentInputOpts{SystemPrompt: "original system prompt"}
	task := &persistence.Task{
		Payload: json.RawMessage(`{"context":{"counterfactual":{"role_prompt_override":{"researcher":"different prompt"}}}}`),
	}
	applyCounterfactualPromptOverride(opts, task, "lead")
	if opts.SystemPrompt != "original system prompt" {
		t.Errorf("mismatched role should leave prompt untouched: %q", opts.SystemPrompt)
	}
}

// TestApplyCounterfactualPromptOverride_NilInputsSafe — nil
// pointers don't panic. Defensive — the helper sits on the
// per-step hot path and nil-safety matters for legacy code paths.
func TestApplyCounterfactualPromptOverride_NilInputsSafe(t *testing.T) {
	applyCounterfactualPromptOverride(nil, nil, "lead")
	applyCounterfactualPromptOverride(&agentInputOpts{}, nil, "lead")
	applyCounterfactualPromptOverride(nil, &persistence.Task{}, "lead")
}

// Note: the executor's budget-override consumers (workflow.go's
// maxVisits + stepTimeout, container.go's VORNIK_LLM_MAX_TOKENS)
// are exercised via blackbox-level integration tests rather than
// here — they live inside loops with deep dependencies on plan +
// roleConfig that aren't worth stubbing for a single override
// check. The override extraction + ResolveModel / ResolvePrompt
// branches above pin the contract; the consumers are thin
// `if override > 0 && override < current` clamps that the
// integration tests cover. The blackbox-package test
// TestMutatePayload_Budget pins the wire shape side; runtime
// effect is verified live (daemon restart + replay smoke test).
