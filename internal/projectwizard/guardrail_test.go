package projectwizard

import (
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

func testBudgetDefaults() config.ComposerBudget {
	return config.ComposerBudget{DailySoftUSD: 1, DailyHardUSD: 3, MonthlySoftUSD: 15, MonthlyHardUSD: 40}
}

func materializeValid(t *testing.T) *materializedBundle {
	t.Helper()
	mb, violations, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("unexpected violations: %v", violations)
	}
	return mb
}

// --- Fill cases: missing safe defaults are filled, never a violation. ---

func TestApplyGuardrails_FillsMissingBudget(t *testing.T) {
	mb := materializeValid(t)
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if len(res.Violations) != 0 {
		t.Fatalf("missing budget must be filled, not a violation: %v", res.Violations)
	}
	if mb.Project.Budget.DailyHardUSD != 3 {
		t.Errorf("budget not filled: %+v", mb.Project.Budget)
	}
	if !anyContains(res.DefaultsApplied, "budget") {
		t.Errorf("expected a 'defaults applied' line for budget, got %v", res.DefaultsApplied)
	}
}

func TestApplyGuardrails_DoesNotOverwriteExplicitBudget(t *testing.T) {
	mb := materializeValid(t)
	mb.Project.Budget.DailyHardUSD = 9
	mb.Project.Budget.DailySoftUSD = 5
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if mb.Project.Budget.DailyHardUSD != 9 {
		t.Errorf("explicit budget must not be overwritten, got %v", mb.Project.Budget)
	}
	if anyContains(res.DefaultsApplied, "budget") {
		t.Error("no fill should be recorded when budget was already set")
	}
}

func TestApplyGuardrails_FillsWorkflowBounds(t *testing.T) {
	mb := materializeValid(t)
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	wf := mb.Workflows[0]
	if wf.MaxStepVisits != defaultMaxStepVisits || wf.MaxIterations != defaultMaxIterations || wf.MaxWallClock != defaultMaxWallClock {
		t.Errorf("workflow bounds not filled: %+v", wf)
	}
	for _, want := range []string{"maxStepVisits", "maxIterations", "maxWallClock"} {
		if !anyContains(res.DefaultsApplied, want) {
			t.Errorf("expected a defaults-applied line mentioning %q, got %v", want, res.DefaultsApplied)
		}
	}
}

func TestApplyGuardrails_FillsRoleMaxTokensWhenZero(t *testing.T) {
	mb := materializeValid(t)
	mb.Swarm.Roles[0].MaxTokens = 0
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if mb.Swarm.Roles[0].MaxTokens != defaultRoleMaxTokens {
		t.Errorf("expected maxTokens filled, got %d", mb.Swarm.Roles[0].MaxTokens)
	}
	if len(res.Violations) != 0 {
		t.Errorf("missing maxTokens must be filled, not a violation: %v", res.Violations)
	}
}

func TestApplyGuardrails_DisablesDelegationSilently(t *testing.T) {
	mb := materializeValid(t)
	mb.Swarm.Roles[0].Permissions.DelegationAllowed = true
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if mb.Swarm.Roles[0].Permissions.DelegationAllowed {
		t.Error("delegation must be forced off for composed roles in v1")
	}
	if len(res.Violations) != 0 {
		t.Errorf("delegation-off is a fill, not a violation: %v", res.Violations)
	}
}

// --- Corrective-reprompt cases: meaning-changing violations are NEVER
// silently fixed. ---

func TestApplyGuardrails_ToolOverreach_ReprompsNotStrips(t *testing.T) {
	mb := materializeValid(t)
	before := append([]string(nil), mb.Swarm.Roles[0].Permissions.AllowedTools...)
	toolViolations := []roleToolViolation{{Role: "researcher", Tool: "run_shell"}}
	res := applyGuardrails(mb, ComposedPlan{}, toolViolations, testBudgetDefaults(), "")
	if len(res.Violations) != 1 || res.Violations[0].Rule != guardrailRuleToolOverreach {
		t.Fatalf("expected exactly one tool_overreach violation, got %v", res.Violations)
	}
	// MUST NOT silently strip the tool — the caller re-prompts instead.
	if len(mb.Swarm.Roles[0].Permissions.AllowedTools) != len(before) {
		t.Error("guardrail pass must never mutate the tool list on a violation")
	}
}

func TestApplyGuardrails_TooManyWorkflows_Violation(t *testing.T) {
	mb := materializeValid(t)
	mb.Workflows = append(mb.Workflows, mb.Workflows[0], mb.Workflows[0])
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if !hasRule(res.Violations, guardrailRuleWorkflowCount) {
		t.Errorf("expected workflow_count violation, got %v", res.Violations)
	}
}

func TestApplyGuardrails_AutonomyBroaderThanConfirmed_Violation(t *testing.T) {
	mb := materializeValid(t)
	mb.Project.Autonomy.Enabled = true
	mb.Project.Autonomy.PollInterval = "1h"
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "24h")
	if !hasRule(res.Violations, guardrailRuleAutonomyBroader) {
		t.Errorf("expected autonomy_broader_than_schedule violation, got %v", res.Violations)
	}
}

func TestApplyGuardrails_AutonomyMatchesConfirmed_NoViolation(t *testing.T) {
	mb := materializeValid(t)
	mb.Project.Autonomy.Enabled = true
	mb.Project.Autonomy.PollInterval = "24h"
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "24h")
	if hasRule(res.Violations, guardrailRuleAutonomyBroader) {
		t.Errorf("matching schedule must not violate, got %v", res.Violations)
	}
}

func TestApplyGuardrails_NoConfirmationYet_SkipsAutonomyCheck(t *testing.T) {
	mb := materializeValid(t)
	mb.Project.Autonomy.Enabled = true
	mb.Project.Autonomy.PollInterval = "1h"
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if hasRule(res.Violations, guardrailRuleAutonomyBroader) {
		t.Error("no confirmation on record yet — this is the commit-time schedule-confirmation gate's job, not a guardrail violation")
	}
}

func TestApplyGuardrails_DroppedApproval_Violation(t *testing.T) {
	mb := materializeValid(t)
	// Give the writer role an outward MCP tool and route straight to it
	// with no approval step in between.
	mb.Swarm.Roles[1].Permissions.AllowedTools = append(mb.Swarm.Roles[1].Permissions.AllowedTools, "mcp__pagedrop__pagedrop_publish_doc")
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if !hasRule(res.Violations, guardrailRuleDroppedApproval) {
		t.Errorf("expected dropped_approval violation, got %v", res.Violations)
	}
}

func TestApplyGuardrails_ApprovalPresent_NoViolation(t *testing.T) {
	mb := materializeValid(t)
	mb.Swarm.Roles[1].Permissions.AllowedTools = append(mb.Swarm.Roles[1].Permissions.AllowedTools, "mcp__pagedrop__pagedrop_publish_doc")
	// Insert an approval step ahead of "write".
	mb.Workflows[0].Steps["gather"] = registry.WorkflowStep{Type: "agent", Role: "researcher", OnSuccess: "approve", OnFail: "failed"}
	mb.Workflows[0].Steps["approve"] = registry.WorkflowStep{Type: "approval", OnSuccess: "write", OnFail: "failed"}
	res := applyGuardrails(mb, ComposedPlan{}, nil, testBudgetDefaults(), "")
	if hasRule(res.Violations, guardrailRuleDroppedApproval) {
		t.Errorf("approval step present — must not violate, got %v", res.Violations)
	}
}

func TestApplyGuardrails_ApprovalBypassRecorded_NoViolation(t *testing.T) {
	mb := materializeValid(t)
	mb.Swarm.Roles[1].Permissions.AllowedTools = append(mb.Swarm.Roles[1].Permissions.AllowedTools, "mcp__pagedrop__pagedrop_publish_doc")
	plan := ComposedPlan{ApprovalsBypassed: []string{"publish step"}}
	res := applyGuardrails(mb, plan, nil, testBudgetDefaults(), "")
	if hasRule(res.Violations, guardrailRuleDroppedApproval) {
		t.Errorf("recorded bypass must suppress the violation, got %v", res.Violations)
	}
}

func TestApplyGuardrails_ReadOnlyMCPToolNotOutward(t *testing.T) {
	if isOutwardMCPTool("mcp__scraper__web_fetch") {
		t.Error("scraper web_fetch is read-only and must not be treated as outward")
	}
	if !isOutwardMCPTool("mcp__pagedrop__pagedrop_publish_doc") {
		t.Error("pagedrop publish is an outward side effect")
	}
	if isOutwardMCPTool("file_read") {
		t.Error("non-mcp tool must never be flagged outward")
	}
}

func hasRule(vs []guardrailViolation, rule string) bool {
	for _, v := range vs {
		if v.Rule == rule {
			return true
		}
	}
	return false
}
