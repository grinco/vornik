package projectwizard

import (
	"fmt"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

// Guardrail bound defaults (design §5.4 — "archetype hints"; mirrors
// the shipped defaults already documented on registry.Workflow's own
// field comments: maxStepVisits 3, maxIterations 20; maxWallClock 1h
// is the documented sensible-global-default from the same file).
const (
	defaultMaxStepVisits = 3
	defaultMaxIterations = 20
	defaultMaxWallClock  = "1h"
	// defaultRoleMaxTokens is a conservative fallback for a role whose
	// MaxTokens is somehow zero post-materialization. In practice every
	// role-library archetype declares runtime.maxTokens > 0 (the
	// library doctor check, rolelibrary.CheckLibrary, enforces it), so
	// this only fires as a defense-in-depth backstop.
	defaultRoleMaxTokens = 4096
)

// Guardrail violation rule labels — also the
// vornik_composer_guardrail_hits_total{rule} metric label values.
const (
	guardrailRuleToolOverreach   = "tool_overreach"
	guardrailRuleAutonomyBroader = "autonomy_broader_than_schedule"
	guardrailRuleDroppedApproval = "dropped_approval"
	guardrailRuleWorkflowCount   = "workflow_count"
)

// guardrailViolation is one meaning-changing violation the design's
// enforcement precedence forbids silently fixing (§5.4): the server
// re-prompts the LLM once naming the violation, then bounces the turn
// into the conversation on a second failure.
type guardrailViolation struct {
	Rule    string
	Message string
}

// guardrailResult is the outcome of one guardrail pass.
type guardrailResult struct {
	// DefaultsApplied lists every mechanical fill in plain language
	// ("defaults applied" transcript line, §5.4).
	DefaultsApplied []string
	// Violations lists every meaning-changing violation found. A
	// non-empty list means the bundle must NOT be treated as valid —
	// the caller re-prompts / bounces rather than proceeding to staged
	// registry validation with a silently-altered bundle.
	Violations []guardrailViolation
}

// applyGuardrails runs the deterministic guardrail pass (design §5.4)
// over an already-materialized bundle.
//
// Mutates mb in place for the FILL side only (missing budget /
// workflow bounds / role maxTokens / delegation-off — mechanical,
// safe, never contradicts what the plan promised). Violations are
// collected, never auto-corrected: the tool-allowlist overreach cases
// are threaded in from materializeBundle (which already has the
// archetype in hand); autonomy breadth, dropped approvals, and the
// workflow-count cap are checked here directly.
//
// confirmedSchedule is the session's previously-confirmed autonomy
// interval (empty when nothing has been confirmed yet, in which case
// the autonomy-breadth check is skipped — first-time schedules are
// gated at commit by the schedule-confirmation check, not here).
func applyGuardrails(mb *materializedBundle, plan ComposedPlan, toolViolations []roleToolViolation, budgetDefaults config.ComposerBudget, confirmedSchedule string) guardrailResult {
	var res guardrailResult
	if mb == nil {
		return res
	}

	// --- Fill, never alter (mechanical) ---
	if mb.Project != nil {
		b := &mb.Project.Budget
		if b.DailySoftUSD == 0 && b.DailyHardUSD == 0 && b.MonthlySoftUSD == 0 && b.MonthlyHardUSD == 0 {
			b.DailySoftUSD = budgetDefaults.DailySoftUSD
			b.DailyHardUSD = budgetDefaults.DailyHardUSD
			b.MonthlySoftUSD = budgetDefaults.MonthlySoftUSD
			b.MonthlyHardUSD = budgetDefaults.MonthlyHardUSD
			res.DefaultsApplied = append(res.DefaultsApplied,
				fmt.Sprintf("budget: filled from composer defaults (daily hard cap $%.2f)", b.DailyHardUSD))
		}
	}
	for _, wf := range mb.Workflows {
		if wf.MaxStepVisits == 0 {
			wf.MaxStepVisits = defaultMaxStepVisits
			res.DefaultsApplied = append(res.DefaultsApplied, fmt.Sprintf("%s.maxStepVisits: filled (%d)", wf.ID, defaultMaxStepVisits))
		}
		if wf.MaxIterations == 0 {
			wf.MaxIterations = defaultMaxIterations
			res.DefaultsApplied = append(res.DefaultsApplied, fmt.Sprintf("%s.maxIterations: filled (%d)", wf.ID, defaultMaxIterations))
		}
		if wf.MaxWallClock == "" {
			wf.MaxWallClock = defaultMaxWallClock
			res.DefaultsApplied = append(res.DefaultsApplied, fmt.Sprintf("%s.maxWallClock: filled (%s)", wf.ID, defaultMaxWallClock))
		}
	}
	if mb.Swarm != nil {
		for i := range mb.Swarm.Roles {
			role := &mb.Swarm.Roles[i]
			if role.MaxTokens == 0 {
				role.MaxTokens = defaultRoleMaxTokens
				res.DefaultsApplied = append(res.DefaultsApplied, fmt.Sprintf("role %s: maxTokens filled (%d)", role.Name, defaultRoleMaxTokens))
			}
			// Delegation off in v1 composed roles (§5.4) — a
			// mechanical clamp, not something the plan's narrative
			// depends on, so it's a fill rather than a violation.
			if role.Permissions.DelegationAllowed || role.Permissions.AutonomousTaskCreation {
				role.Permissions.DelegationAllowed = false
				role.Permissions.AutonomousTaskCreation = false
				res.DefaultsApplied = append(res.DefaultsApplied, fmt.Sprintf("role %s: delegation disabled (composed roles are non-delegating in v1)", role.Name))
			}
		}
	}

	// --- Corrective re-prompt, never silent strip (meaning-changing) ---
	for _, v := range toolViolations {
		res.Violations = append(res.Violations, guardrailViolation{
			Rule:    guardrailRuleToolOverreach,
			Message: fmt.Sprintf("role %q would carry tool %q, which is outside its archetype's allowlist", v.Role, v.Tool),
		})
	}
	if len(mb.Workflows) > 2 {
		res.Violations = append(res.Violations, guardrailViolation{
			Rule:    guardrailRuleWorkflowCount,
			Message: fmt.Sprintf("bundle carries %d workflows; v1 allows at most 2", len(mb.Workflows)),
		})
	}
	if mb.Project != nil && mb.Project.Autonomy.Enabled && confirmedSchedule != "" {
		// schedulesEquivalent (composer_engine.go, companion review
		// finding 2) normalizes through time.ParseDuration rather than
		// comparing raw strings, so a re-formatted-but-identical
		// cadence ("24h" vs "1440m") isn't flagged as a violation —
		// while still failing safe (never matching) if either side is
		// unparseable, so a genuine change is never silently waved
		// through.
		if got := mb.Project.Autonomy.PollInterval; !schedulesEquivalent(got, confirmedSchedule) {
			res.Violations = append(res.Violations, guardrailViolation{
				Rule:    guardrailRuleAutonomyBroader,
				Message: fmt.Sprintf("autonomy schedule %q no longer matches the previously confirmed schedule %q — re-confirmation required", got, confirmedSchedule),
			})
		}
	}
	if v := checkDroppedApprovals(mb, plan); v != nil {
		res.Violations = append(res.Violations, *v)
	}

	return res
}

// knownReadOnlyMCPPrefixes lists MCP tool prefixes the composer
// treats as read-only / research (not an outward side effect). Every
// OTHER mcp__ tool is conservatively treated as outward-side-effecting
// for the dropped-approval check — matching the design's stance that
// guardrail defaults are stricter than hand-authored config (§5.4):
// an unrecognised MCP tool is assumed to write until proven otherwise,
// not the reverse.
var knownReadOnlyMCPPrefixes = []string{"mcp__scraper__"}

// outwardSystemHandlers names system-step handlers with an outward
// side effect (posting to an external system).
var outwardSystemHandlers = map[string]bool{
	"forge.post_review":         true,
	"forge.open_change_request": true,
}

func isOutwardMCPTool(tool string) bool {
	if !agenttools.IsMCPTool(tool) {
		return false
	}
	for _, p := range knownReadOnlyMCPPrefixes {
		if len(tool) >= len(p) && tool[:len(p)] == p {
			return false
		}
	}
	return true
}

// isOutwardSideEffecting reports whether a workflow step has an
// outward side effect: a system step calling a known-outward handler,
// or an agent step whose role carries an outward MCP tool.
func isOutwardSideEffecting(st registry.WorkflowStep, roleTools map[string][]string) bool {
	switch st.Type {
	case "system":
		return outwardSystemHandlers[st.Handler]
	case "agent":
		for _, tool := range roleTools[st.Role] {
			if isOutwardMCPTool(tool) {
				return true
			}
		}
	}
	return false
}

// checkDroppedApprovals scans every workflow for an outward-side-
// effecting step with no immediately-preceding `approval` step. A
// non-empty plan.ApprovalsBypassed is treated as the operator's
// explicit, transcript-recorded acknowledgement (design §5.2/§5.4 —
// "persisted with the session as the audit trail for the removal"),
// so a drop is only a violation when NOTHING was recorded as
// bypassed.
func checkDroppedApprovals(mb *materializedBundle, plan ComposedPlan) *guardrailViolation {
	if mb == nil || len(plan.ApprovalsBypassed) > 0 {
		return nil
	}
	roleTools := map[string][]string{}
	if mb.Swarm != nil {
		for _, r := range mb.Swarm.Roles {
			roleTools[r.Name] = r.Permissions.AllowedTools
		}
	}
	for _, wf := range mb.Workflows {
		predecessor := map[string]string{}
		for id, st := range wf.Steps {
			if st.OnSuccess != "" {
				predecessor[st.OnSuccess] = id
			}
		}
		for id, st := range wf.Steps {
			if !isOutwardSideEffecting(st, roleTools) {
				continue
			}
			predID, hasPred := predecessor[id]
			if hasPred && wf.Steps[predID].Type == "approval" {
				continue
			}
			return &guardrailViolation{
				Rule:    guardrailRuleDroppedApproval,
				Message: fmt.Sprintf("workflow %q step %q has an outward side effect with no preceding approval step", wf.ID, id),
			}
		}
	}
	return nil
}
