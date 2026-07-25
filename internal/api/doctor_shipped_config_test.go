package api

import (
	"strings"
	"testing"
)

// Onboarding-hardening task 13: a fresh install's `vornikctl doctor`
// showed cosmetic warnings against SHIPPED config (not an operator's
// deployed project), which makes a brand-new install look broken.
// These tests point the doctor checks directly at the repo's real
// configs/ tree (the same tree a fresh install ships) and lock in
// that the three specific findings named in the task brief are gone.
//
// Deliberately narrow: each assertion targets only the exact
// file/role the task fixed. Other pre-existing warnings against
// OTHER shipped or operator-deployed config (e.g.
// ibkr-trader-swarm/strategist, companion-example-swarm/analyst) are
// out of scope here and must keep firing if they still apply
// elsewhere — this test must not become a blanket "doctor is 100%
// clean" assertion that breaks the moment an unrelated warning is
// legitimately added.

const shippedConfigsDir = "../../configs"

// TestShippedConfigs_RolePromptSanity_AssistantSwarmClean guards the
// role_prompt_sanity warnings against configs/swarms/assistant-swarm.md:
// researcher had memory_search in allowedTools but its effective
// systemPrompt never mentioned it; writer's prompt mentioned
// memory_search but it was missing from allowedTools.
func TestShippedConfigs_RolePromptSanity_AssistantSwarmClean(t *testing.T) {
	h := &DoctorHandlers{configDir: shippedConfigsDir}
	check := h.checkRolePromptSanity()
	for _, item := range check.Items {
		if strings.HasPrefix(item, "assistant-swarm/researcher:") && strings.Contains(item, "memory_search") {
			t.Errorf("assistant-swarm/researcher still has a memory_search role_prompt_sanity finding: %s", item)
		}
		if strings.HasPrefix(item, "assistant-swarm/writer:") && strings.Contains(item, "memory_search") {
			t.Errorf("assistant-swarm/writer still has a memory_search role_prompt_sanity finding: %s", item)
		}
	}
}

// TestShippedConfigs_EvalSuiteLint_BasicSwarmProjectLoaded guards
// the eval_suite_lint warning against configs/evals/basic-swarm.json,
// which referenced project_id "test-project" — never loaded in the
// shipped registry.
func TestShippedConfigs_EvalSuiteLint_BasicSwarmProjectLoaded(t *testing.T) {
	h := &DoctorHandlers{configDir: shippedConfigsDir}
	check := h.checkEvalSuiteLint()
	for _, item := range check.Items {
		if strings.HasPrefix(item, "basic-swarm.json:") {
			t.Errorf("basic-swarm.json still has an eval_suite_lint finding: %s", item)
		}
	}
}

// TestShippedConfigs_WorkflowMdShape_TradingClean guards the
// workflow_md_shape (author_missing / license_missing) warnings
// against configs/workflows/trading.md.
func TestShippedConfigs_WorkflowMdShape_TradingClean(t *testing.T) {
	h := &DoctorHandlers{configDir: shippedConfigsDir}
	check := h.checkWorkflowMDShape()
	for _, item := range check.Items {
		if strings.HasPrefix(item, "trading.md:") {
			t.Errorf("trading.md still has a workflow_md_shape finding: %s", item)
		}
	}
}
