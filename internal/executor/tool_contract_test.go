package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func auditRow(tool, outcome, class string) *persistence.ToolAuditEntry {
	return &persistence.ToolAuditEntry{ToolName: tool, Outcome: outcome, OutcomeClass: class}
}

// THE regression test for the 2026-08-25 P0.
//
// An outreach step whose mandatory Jira duplicate check 401'd must NOT report
// success. On the pre-fix build this exact shape produced a task that ran 174
// seconds, reported no error, and completed.
func TestAuthClassFailureFailsTheStep(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent", Role: "outreach"}
	entries := []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__getAccessibleAtlassianResources", "error", "auth"),
	}

	v := checkToolContract(step, "outreach", "vornik-marketing", entries)
	if v == nil {
		t.Fatal("a step whose tool was rejected for authentication must not report success")
	}
	if !v.AuthClass {
		t.Error("the violation must be marked auth-class — it is a deployment condition, not an agent mistake")
	}
	// The message has to carry the cure. The original incident's payload DID
	// say "re-authenticate" and nobody read it; this lands in the step
	// failure, the task error and the log.
	for _, want := range []string{"atlassian", "vornikctl mcp connect", "-p vornik-marketing"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message %q does not contain %q", v.Message, want)
		}
	}
}

// No opt-in required: a step that declares nothing is still protected. This is
// the property that covers every existing workflow without a config change.
func TestAuthContractNeedsNoDeclaration(t *testing.T) {
	bare := &registry.WorkflowStep{Type: "agent"}
	if checkToolContract(bare, "s", "p", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJiraIssuesUsingJql", "error", "auth"),
	}) == nil {
		t.Fatal("a step that declares no contract must still fail on an auth-class failure")
	}
}

// A 401 followed by a successful retry of the SAME tool is not a failure — the
// credential refreshed and the work got done. Without this the one-shot
// refresh-and-retry would fail every step it rescued.
func TestLaterSuccessClearsAnAuthFailure(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent"}
	entries := []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJira", "error", "auth"),
		auditRow("mcp__atlassian__searchJira", "ok", ""),
	}
	if v := checkToolContract(step, "s", "p", entries); v != nil {
		t.Fatalf("a recovered call must not fail the step: %s", v.Message)
	}
}

// Scoped to the SAME tool: a different tool succeeding says nothing about the
// one that was rejected. A step that genuinely degrades across connectors opts
// out explicitly instead.
func TestADifferentToolSucceedingDoesNotClear(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent"}
	entries := []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJira", "error", "auth"),
		auditRow("mcp__github__search", "ok", ""),
	}
	if checkToolContract(step, "s", "p", entries) == nil {
		t.Fatal("another tool succeeding must not excuse a rejected credential")
	}
}

// The finding-A opt-out: routing only.
func TestAuthFailureModeContinueDoesNotFailTheStep(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent", AuthFailureMode: "continue"}
	entries := []*persistence.ToolAuditEntry{
		auditRow("mcp__slack__post", "error", "auth"),
		auditRow("mcp__email__send", "ok", ""),
	}
	if v := checkToolContract(step, "notify", "p", entries); v != nil {
		t.Fatalf("auth_failure_mode: continue must not fail the step: %s", v.Message)
	}
}

// Case and whitespace are forgiven, because that is what operators type.
func TestAuthFailureModeIsNormalised(t *testing.T) {
	for _, mode := range []string{"continue", "Continue", " CONTINUE "} {
		step := &registry.WorkflowStep{Type: "agent", AuthFailureMode: mode}
		if v := checkToolContract(step, "s", "p", []*persistence.ToolAuditEntry{
			auditRow("mcp__slack__post", "error", "auth"),
		}); v != nil {
			t.Errorf("mode %q should have been accepted as continue", mode)
		}
	}
}

// A NON-auth failure does not trip rule 1. A 500 or a rate limit is the
// vendor's problem and has its own handling; conflating them would make the
// auth signal meaningless.
func TestNonAuthFailureDoesNotTripTheAuthRule(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent"}
	for _, class := range []string{"server", "rate_limit", "transport", "invalid_request", ""} {
		entries := []*persistence.ToolAuditEntry{auditRow("mcp__x__y", "error", class)}
		if v := checkToolContract(step, "s", "p", entries); v != nil {
			t.Errorf("class %q must not trip the auth rule: %s", class, v.Message)
		}
	}
}

// A pre-migration row carries no class and must not be read as an auth failure
// OR as a success.
func TestUnknownOutcomeIsInert(t *testing.T) {
	step := &registry.WorkflowStep{Type: "agent"}
	if v := checkToolContract(step, "s", "p", []*persistence.ToolAuditEntry{
		{ToolName: "mcp__atlassian__searchJira"},
	}); v != nil {
		t.Fatalf("an unclassified row must not fail the step: %s", v.Message)
	}
}

func TestRequireToolsUnsatisfied(t *testing.T) {
	step := &registry.WorkflowStep{
		Type:         "agent",
		RequireTools: []string{"mcp__atlassian__searchJiraIssuesUsingJql"},
	}
	v := checkToolContract(step, "outreach", "vornik-marketing", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__createJiraIssue", "ok", ""),
	})
	if v == nil {
		t.Fatal("a declared mandatory tool that never succeeded must fail the step")
	}
	// The prefix is load-bearing: it is what the shape-retry layer recognises,
	// giving the agent one corrective attempt before on_fail.
	if !strings.HasPrefix(v.Message, "schema violation:") {
		t.Errorf("message must carry the shape-retry prefix, got %q", v.Message)
	}
	if v.AuthClass {
		t.Error("an uncalled tool is not an auth-class violation")
	}
}

func TestRequireToolsSatisfied(t *testing.T) {
	step := &registry.WorkflowStep{
		Type:         "agent",
		RequireTools: []string{"mcp__atlassian__searchJiraIssuesUsingJql"},
	}
	if v := checkToolContract(step, "outreach", "p", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJiraIssuesUsingJql", "ok", ""),
	}); v != nil {
		t.Fatalf("a satisfied contract must pass: %s", v.Message)
	}
}

// A FAILED call does not satisfy a contract. This is the whole point: the
// outreach agent DID call its duplicate check, and the call 401'd.
func TestAFailedCallDoesNotSatisfyRequireTools(t *testing.T) {
	step := &registry.WorkflowStep{
		Type:            "agent",
		RequireTools:    []string{"mcp__atlassian__searchJiraIssuesUsingJql"},
		AuthFailureMode: "continue", // isolate rule 2 from rule 1
	}
	if checkToolContract(step, "outreach", "p", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJiraIssuesUsingJql", "error", "auth"),
	}) == nil {
		t.Fatal("a failed call must not satisfy require_tools")
	}
}

// An author may name either the qualified or the bare form, matching how they
// already write allowedTools.
func TestRequireToolsMatchesEitherNameShape(t *testing.T) {
	cases := []struct{ declared, recorded string }{
		{"searchJiraIssuesUsingJql", "mcp__atlassian__searchJiraIssuesUsingJql"},
		{"mcp__atlassian__searchJiraIssuesUsingJql", "searchJiraIssuesUsingJql"},
		{"mcp__atlassian__searchJiraIssuesUsingJql", "mcp__atlassian__searchJiraIssuesUsingJql"},
	}
	for _, c := range cases {
		step := &registry.WorkflowStep{Type: "agent", RequireTools: []string{c.declared}}
		if v := checkToolContract(step, "s", "p", []*persistence.ToolAuditEntry{
			auditRow(c.recorded, "ok", ""),
		}); v != nil {
			t.Errorf("declared %q vs recorded %q should match: %s", c.declared, c.recorded, v.Message)
		}
	}
}

// The auth rule runs FIRST: "the credential was rejected" is a better
// diagnosis than "the tool was not called", and it is the cause of the other.
func TestAuthDiagnosisWinsOverUncalledTool(t *testing.T) {
	step := &registry.WorkflowStep{
		Type:         "agent",
		RequireTools: []string{"mcp__atlassian__searchJira"},
	}
	v := checkToolContract(step, "s", "vornik-marketing", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJira", "error", "auth"),
	})
	if v == nil {
		t.Fatal("expected a violation")
	}
	if !v.AuthClass {
		t.Fatalf("the auth diagnosis must win, got %q", v.Message)
	}
}

// A step that declares nothing and whose tools all worked must be untouched —
// the back-compat guard for every workflow that existed before this.
func TestCleanStepIsUnaffected(t *testing.T) {
	if v := checkToolContract(&registry.WorkflowStep{Type: "agent"}, "s", "p", []*persistence.ToolAuditEntry{
		auditRow("mcp__atlassian__searchJira", "ok", ""),
		auditRow("file_read", "ok", ""),
	}); v != nil {
		t.Fatalf("a clean step must pass: %s", v.Message)
	}
	if v := checkToolContract(&registry.WorkflowStep{Type: "agent"}, "s", "p", nil); v != nil {
		t.Fatalf("a step with no tool calls at all must pass: %s", v.Message)
	}
}
