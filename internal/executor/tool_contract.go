package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The post-step TOOL contract: what a step's recorded tool calls have to look
// like before the step is allowed to report success.
//
// It exists because prose-mandatory is not mandatory. On 2026-08-25 an outreach
// agent on vornik-marketing was told in its PROMPT that a Jira duplicate check
// was mandatory. The Atlassian connector had lost auth, the check 401'd, and
// the agent did the right thing — refused to file, wrote a draft, spelled out
// what a human must do. The task then ran 174 seconds and COMPLETED, because
// nothing in the workflow could say "that tool had to work". A well-behaved
// agent narrating a connector failure was indistinguishable from a task with
// nothing to file.
//
// Two rules, checked in this order, because "the credential was rejected" is a
// better diagnosis than "the tool was not called" and one causes the other:
//
//  1. AUTH — any auth-class failure with no later success for the same tool
//     fails the step. No opt-in: nobody was going to declare a contract for a
//     failure they did not know was happening. A step may opt OUT of the
//     ROUTING with auth_failure_mode: continue, which changes nothing about
//     what is recorded or alerted.
//  2. REQUIRE_TOOLS — each declared tool needs at least one successful call.
//
// Design: https://docs.vornik.io §3.3

// toolContractViolation is a failed contract, rendered as the agentError string
// the executor already routes through its shape-retry and on_fail machinery.
type toolContractViolation struct {
	// Message is the operator-facing text, including the recovery command
	// where there is one.
	Message string
	// AuthClass is true when the violation was caused by a credential
	// rejection rather than an uncalled tool. The caller logs it distinctly:
	// it is a deployment condition, not an agent mistake.
	AuthClass bool
}

// checkToolContract evaluates a step's contract against the tool_audit_log rows
// recorded for that step. Returns nil when the contract holds — including when
// the step declares nothing, which is every step that existed before this.
//
// entries must already be scoped to this execution AND this step: a recover
// step inheriting the failed research step's rows would re-fail on somebody
// else's 401.
func checkToolContract(step *registry.WorkflowStep, stepID string, projectID string, entries []*persistence.ToolAuditEntry) *toolContractViolation {
	if step == nil {
		return nil
	}
	if v := checkAuthFailures(step, projectID, entries); v != nil {
		return v
	}
	return checkRequiredTools(step, stepID, entries)
}

// checkAuthFailures implements rule 1.
//
// The "no later success for the same tool" clause is what makes this safe for
// retries: a call that 401s and then succeeds after the credential refreshes is
// not a failure, and a workflow whose on_fail routes to a recovery step still
// gets there. It is scoped to the SAME tool deliberately — a different tool
// succeeding says nothing about this one. A step that genuinely degrades across
// connectors says so with auth_failure_mode: continue.
func checkAuthFailures(step *registry.WorkflowStep, projectID string, entries []*persistence.ToolAuditEntry) *toolContractViolation {
	if !step.FailsOnAuthError() {
		return nil
	}
	// Walk in recorded order, tracking the LAST outcome per tool. Entries
	// arrive newest-last from the repository's created_at ordering.
	lastAuthFailure := map[string]*persistence.ToolAuditEntry{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		switch {
		case e.OutcomeClass == string(failureClassAuth):
			lastAuthFailure[e.ToolName] = e
		case e.Outcome == outcomeOK:
			// A later success clears an earlier rejection for this tool.
			delete(lastAuthFailure, e.ToolName)
		}
	}
	if len(lastAuthFailure) == 0 {
		return nil
	}

	tools := make([]string, 0, len(lastAuthFailure))
	for name := range lastAuthFailure {
		tools = append(tools, name)
	}
	sort.Strings(tools)

	servers := connectorNames(tools)
	recovery := recoveryHint(servers, projectID)
	return &toolContractViolation{
		AuthClass: true,
		Message: fmt.Sprintf(
			"connector auth failure: %s was rejected (HTTP 401/403) and never succeeded in this step — "+
				"the connector's OAuth grant needs an operator reconnect. %s",
			strings.Join(quoteAll(tools), ", "), recovery),
	}
}

// checkRequiredTools implements rule 2.
func checkRequiredTools(step *registry.WorkflowStep, stepID string, entries []*persistence.ToolAuditEntry) *toolContractViolation {
	if len(step.RequireTools) == 0 {
		return nil
	}
	succeeded := map[string]bool{}
	for _, e := range entries {
		if e == nil || e.Outcome != outcomeOK {
			continue
		}
		succeeded[e.ToolName] = true
		// Also index the bare segment, so a contract written either way is
		// satisfied by a row recorded either way.
		if bare := bareToolName(e.ToolName); bare != e.ToolName {
			succeeded[bare] = true
		}
	}

	var missing []string
	for _, want := range step.RequireTools {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		if succeeded[want] || succeeded[bareToolName(want)] {
			continue
		}
		missing = append(missing, want)
	}
	if len(missing) == 0 {
		return nil
	}
	// "schema violation:" is load-bearing — it is the prefix the shape-retry
	// layer recognises, so the agent gets one corrective attempt before the
	// step routes to on_fail. Same contract as require_output_glob.
	return &toolContractViolation{
		Message: fmt.Sprintf(
			"schema violation: tool contract for step %q not met — %s did not complete successfully during this step. "+
				"You MUST call the declared tool(s) successfully before finishing.",
			stepID, strings.Join(quoteAll(missing), ", ")),
	}
}

const (
	outcomeOK        = "ok"
	failureClassAuth = "auth"
)

// Tool-name matching reuses bareToolName from tool_grant_prompt.go rather than
// growing a second implementation — it already handles both the MCP ("__") and
// function-namespace (".") conventions, which is exactly the two-shape rule the
// role allowlist uses. An author writes what they already write in
// allowedTools.

// connectorNames extracts the distinct MCP server names from qualified tool
// names, so the recovery hint can say WHICH connector to reconnect rather than
// leaving the operator to work it out from a tool name.
func connectorNames(tools []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tools {
		if !strings.HasPrefix(t, "mcp__") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(t, "mcp__"), "__", 2)
		if len(parts) != 2 || parts[0] == "" || seen[parts[0]] {
			continue
		}
		seen[parts[0]] = true
		out = append(out, parts[0])
	}
	sort.Strings(out)
	return out
}

// recoveryHint renders the command that actually fixes this.
//
// An error that names a condition without naming its cure is how the original
// incident stayed invisible: the agent's payload DID say "re-authenticate", and
// nobody read it. This line goes into the step's failure, the task's error, and
// the log — the places an operator already looks.
func recoveryHint(servers []string, projectID string) string {
	if len(servers) == 0 {
		return "Reconnect it with: vornikctl mcp connect <server>" + projectFlag(projectID)
	}
	cmds := make([]string, 0, len(servers))
	for _, s := range servers {
		cmds = append(cmds, "vornikctl mcp connect "+s+projectFlag(projectID))
	}
	return "Recover with: " + strings.Join(cmds, " && ")
}

func projectFlag(projectID string) string {
	if projectID == "" {
		return ""
	}
	return " -p " + projectID
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// evaluateToolContract reads THIS step's tool_audit_log rows and applies the
// contract.
//
// Fails OPEN on every plumbing gap — no audit repo, no execution id, a read
// error. That is deliberate and matches the surrounding contract checks: this
// gate exists to surface a connector failure, and turning a database blip into
// a failed step would replace one silent problem with a noisy wrong one. A read
// failure is logged at Warn so the gap is visible rather than assumed clean.
func (e *Executor) evaluateToolContract(
	ctx context.Context,
	execution *persistence.Execution,
	task *persistence.Task,
	step *registry.WorkflowStep,
	stepID string,
) *toolContractViolation {
	if e == nil || step == nil || e.auditRepo == nil || execution == nil || execution.ID == "" {
		return nil
	}
	// Nothing to check for a step with neither rule in play. The auth rule
	// applies to every step, so this is only the explicit opt-out plus no
	// declared tools.
	if !step.FailsOnAuthError() && len(step.RequireTools) == 0 {
		return nil
	}

	execID := execution.ID
	filter := persistence.ToolAuditFilter{ExecutionID: &execID, PageSize: 500}
	if stepID != "" {
		sid := stepID
		filter.StepID = &sid
	}
	// Step-scoped for the same reason the verifier's fetch is (2026-05-26):
	// without it a recover step inherits the failed step's rows and re-fails on
	// somebody else's 401.
	entries, err := e.auditRepo.List(ctx, filter)
	if err != nil {
		e.logger.Warn().Err(err).
			Str("execution_id", execID).
			Str("step", stepID).
			Msg("tool contract: audit fetch failed; skipping the check for this step")
		return nil
	}

	projectID := ""
	if task != nil {
		projectID = task.ProjectID
	}
	return checkToolContract(step, stepID, projectID, entries)
}
