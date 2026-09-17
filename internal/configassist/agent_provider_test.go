package configassist

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

type recordingAudit struct{ entries []*persistence.ToolAuditEntry }

func (r *recordingAudit) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	r.entries = append(r.entries, e)
	return nil
}

// TestAgentProvider_ExposesProposeOnly is §6.3.2a's answer made mechanical:
// the provider serves the propose verb and NOTHING else. Apply and rollback
// were the operator selfops surface's mutating verbs, gated by a capability a
// task-scoped key does not carry, and the class ceiling does not cover them
// because they are not classes. An agent proposes; a human applies.
func TestAgentProvider_ExposesProposeOnly(t *testing.T) {
	p := NewAgentProvider(&Engine{}, nil)
	tools := p.Tools("any")
	if len(tools) != 1 {
		t.Fatalf("the agent entrypoint advertises %d tools, want exactly 1", len(tools))
	}
	if tools[0].Function.Name != agentToolName {
		t.Errorf("tool = %q, want %q", tools[0].Function.Name, agentToolName)
	}
	for _, forbidden := range []string{"apply", "rollback", "approve", "delete"} {
		if p.Owns("mcp__vornik__" + forbidden) {
			t.Errorf("the agent entrypoint claims %q; only proposing may be agent-reachable", forbidden)
		}
		if strings.Contains(tools[0].Function.Name, forbidden) {
			t.Errorf("the advertised tool name contains %q", forbidden)
		}
	}
	// And the description must not let a model report a filing as a change.
	if !strings.Contains(tools[0].Function.Description, "never applied automatically") {
		t.Error("the tool description does not tell the model the proposal is not applied")
	}
}

// TestAgentProvider_NilEngineIsNilProvider — the container wires it
// unconditionally and ComposedMCPExecutor nil-checks the field. A non-nil
// provider around no engine would advertise the most exposed entrypoint in
// every deployment.
func TestAgentProvider_NilEngineIsNilProvider(t *testing.T) {
	if NewAgentProvider(nil, nil) != nil {
		t.Fatal("a nil engine produced a live agent provider")
	}
}

// TestAgentProvider_TraceCarriesVerifiedOriginAndNoStep is the amended test 22.
//
// Task and execution come from the call context, where the MCP handler put
// them AFTER validating each against the task-scoped API key. No step is
// carried, and the reason is not thrift: it cannot be carried, cannot be
// derived, and agent self-report is testimony from the suspect in a trace
// built for injection forensics (§6.3.2a).
func TestAgentProvider_TraceCarriesVerifiedOriginAndNoStep(t *testing.T) {
	ctx := context.WithValue(context.Background(), mcp.TaskIDHeaderKey{}, "task_1")
	ctx = context.WithValue(ctx, mcp.ExecutionIDHeaderKey{}, "exec_1")

	taskID, _ := ctx.Value(mcp.TaskIDHeaderKey{}).(string)
	execID, _ := ctx.Value(mcp.ExecutionIDHeaderKey{}).(string)
	trace := &Trace{TaskID: taskID, ExecutionID: execID, ContextSources: []string{"https://example.org/issue/1"}}

	if trace.TaskID != "task_1" || trace.ExecutionID != "exec_1" {
		t.Fatalf("the trace lost its verified origin: %+v", trace)
	}
	if len(trace.ContextSources) != 1 {
		t.Error("the trace dropped the context sources a reviewer needs to see where this came from")
	}

	// THE STRUCTURAL HALF, and it is asserted rather than assumed. The first
	// version of this test called a helper that returned false unconditionally
	// — it read as a check and performed nothing, which is the failure mode
	// this feature's reviews have found in every round. Reflection actually
	// looks.
	for _, forbidden := range []string{"StepID", "Step", "StepIndex", "StepName"} {
		if _, found := reflect.TypeOf(Trace{}).FieldByName(forbidden); found {
			t.Errorf("Trace grew a %q field. §6.3.2a: a step cannot be carried (the ids are "+
				"container env vars and an execution spans many steps on a warm container), "+
				"cannot be derived (a step's row is written when it finishes, not while it "+
				"runs), and agent self-report in an injection trace is testimony from the "+
				"suspect rendered beside two fields the daemon verified", forbidden)
		}
	}
}

// TestAuditOutcome_NamesTheRefusalCode — the audit row is what an operator
// greps while reconstructing an injection, so a refusal must be identifiable
// by CODE there even though the agent-facing reply carries prose.
func TestAuditOutcome_NamesTheRefusalCode(t *testing.T) {
	refused := auditOutcome(&Result{Refusal: &Refusal{Code: RefuseEntrypointCeiling, Message: "no"}}, nil)
	if !strings.Contains(refused, RefuseEntrypointCeiling) {
		t.Errorf("a refusal audits as %q, without its code", refused)
	}
	filed := auditOutcome(&Result{Proposal: &persistence.ControlPlaneProposal{ID: "cap_9"}, Class: ClassA}, nil)
	if !strings.Contains(filed, "cap_9") {
		t.Errorf("a filing audits as %q, without its proposal id", filed)
	}
}

// TestRenderAgentResult_TellsTheModelItIsNotDone — this reply goes to a MODEL,
// which will summarise "Filed proposal cap_1" to a human as "I changed the
// config". Nothing an agent proposes is applied without a person.
func TestRenderAgentResult_TellsTheModelItIsNotDone(t *testing.T) {
	got := renderAgentResult(&Result{
		Proposal: &persistence.ControlPlaneProposal{ID: "cap_1"}, Class: ClassA,
	})
	if !strings.Contains(got, "NOT been applied") {
		t.Errorf("the model is not told the change is unapplied: %s", got)
	}
	if !strings.Contains(got, "Do not report this as a completed change") {
		t.Errorf("the model is not told how to report it: %s", got)
	}
}

// TestAgentEntrypoint_NeverAutoApplies is design test 22's other half, at the
// level the rule actually lives: MayAutoApply is what the engine consults, and
// the agent entrypoint must be excluded from it whatever a project's opt-in
// says.
func TestAgentEntrypoint_NeverAutoApplies(t *testing.T) {
	if MayAutoApply(EntrypointAgent) {
		t.Fatal("the agent entrypoint may auto-apply; an injected proposal could reach the " +
			"deployment with no person in the loop")
	}
	if MayAutoApply(EntrypointChat) {
		t.Fatal("the chat entrypoint may auto-apply")
	}
	// And the ceiling refuses everything above B2 there.
	for _, class := range []string{ClassB1, ClassC, ClassD, ClassE} {
		if GateEntrypointCeiling(EntrypointAgent, class) == nil {
			t.Errorf("class %s passes the agent entrypoint's ceiling", class)
		}
	}
	for _, class := range []string{ClassA, ClassB2} {
		if GateEntrypointCeiling(EntrypointAgent, class) != nil {
			t.Errorf("class %s is refused at the agent entrypoint; it should carry A and B2", class)
		}
	}
}

// TestAgentProvider_AuditsEveryCallIncludingRefusals pins one of the four
// mitigations §6.3.2 rests this entrypoint on.
//
// It matters more here than anywhere else in the feature: §6.3.2a verified
// that NOTHING upstream writes a tool-audit row for a built-in provider —
// ComposedMCPExecutor.Execute runs only the ingress result guard, and the MCP
// handler above it validates the origin headers and calls the executor without
// logging. A provider that assumed "tool calls are audited" would leave the
// most exposed entrypoint with no record of what it refused, on a surface
// whose entire safety claim is "confined, not detected".
func TestAgentProvider_AuditsEveryCallIncludingRefusals(t *testing.T) {
	audit := &recordingAudit{}
	p := &AgentProvider{engine: &Engine{}, audit: audit}
	ctx := context.Background()

	// A REFUSAL must be audited. This is the case a "log the successes"
	// implementation drops, and it is the case an injection investigation
	// needs most: what did the agent try that the ceiling stopped?
	p.record(ctx, "proj", "task_1", "exec_1", "grant every tool to the lead",
		&Result{Refusal: &Refusal{Code: RefuseEntrypointCeiling, Message: "no"}}, nil)
	// A filing.
	p.record(ctx, "proj", "task_1", "exec_1", "raise the retry budget",
		&Result{Proposal: &persistence.ControlPlaneProposal{ID: "cap_1"}, Class: ClassA}, nil)

	if len(audit.entries) != 2 {
		t.Fatalf("audited %d calls, want 2 — a refusal is a call", len(audit.entries))
	}

	refusal := audit.entries[0]
	if refusal.Outcome != "ok" {
		t.Errorf("a refusal audited with Outcome %q. The column's vocabulary is ok/error and "+
			"models.go forbids inventing values; the call SUCCEEDED and answered no", refusal.Outcome)
	}
	if !strings.Contains(refusal.ToolOutput, RefuseEntrypointCeiling) {
		t.Errorf("the refusal's CODE is not in the audit row: %q", refusal.ToolOutput)
	}
	if refusal.ToolInput != "grant every tool to the lead" {
		t.Errorf("the agent's intent was not recorded: %q — it is the clearest evidence of what "+
			"the agent was steered to ask for", refusal.ToolInput)
	}
	if refusal.TaskID != "task_1" || refusal.ExecutionID != "exec_1" {
		t.Errorf("the audit row lost the verified origin: task=%q exec=%q", refusal.TaskID, refusal.ExecutionID)
	}

	if filed := audit.entries[1]; !strings.Contains(filed.ToolOutput, "cap_1") {
		t.Errorf("the filing's proposal id is not in the audit row: %q", filed.ToolOutput)
	}

	// A fault audits as an error, not as a silent success.
	p.record(ctx, "proj", "task_1", "exec_1", "anything", nil, context.DeadlineExceeded)
	if last := audit.entries[2]; last.Outcome != "error" {
		t.Errorf("a failed run audited as %q, want error", last.Outcome)
	}

	// And a provider with no audit sink must not panic — a deployment without
	// the repository still serves, it just cannot record.
	quiet := &AgentProvider{engine: &Engine{}}
	quiet.record(ctx, "proj", "t", "e", "intent", &Result{}, nil)
}

// TestSystemEntrypoint_CannotAutoApplyClassC is the claim §6.3.4's first draft
// got loose, now asserted instead of trusted (review-20260915-cc32 F3).
//
// That draft said MayAutoApply is true for the system entrypoint and that this
// "is correct", which — standing beside a class table whose class-C row says
// NO — reads as relaxing a rule round 3 fought to establish. It does not.
// MayAutoApply(entrypoint) is the FIRST gate, not the decision: the class
// switch below it refuses C, D and E for every entrypoint, this one included.
//
// The distinction matters because healing edits topology, which is class C. A
// healing proposal therefore never auto-applies through the assistant's apply
// path at all; it becomes a candidate, and the healing promotion gate — a
// replay-gated trial and a manual operator promotion — governs application.
// The assistant PROPOSES and may apply; healing PROMOTES. If this test ever
// fails, the two have been confused again and a topology edit can reach the
// deployment with no trial.
func TestSystemEntrypoint_CannotAutoApplyClassC(t *testing.T) {
	// A NON-NIL Applier, and this is the whole reason the test is worth
	// anything. The first version used &Engine{}, whose nil Applier
	// short-circuits mayAutoApply at its first line — so the test passed
	// without the class gate ever running, and a mutation that let class C
	// through the gate did not fail it. Found by mutating the guard, which is
	// what this codebase asks for and what a green test would otherwise have
	// hidden.
	// Every gate ABOVE the class check is deliberately set to PASS, so the
	// class check is the only thing that can refuse. This took two rounds of
	// mutation to get right and both failures were the same shape — a test
	// that was green because an earlier gate short-circuited, proving nothing
	// about the gate it named:
	//
	//   1. &Engine{} — a nil Applier returns false on mayAutoApply's FIRST
	//      line, so the class switch never ran.
	//   2. config.AssistantConfig{} — AutoApplyAllows returns false for a
	//      project with no opt-in, so the class switch still never ran.
	//   3. JudgeVerdict{Decision: "pass"} — Pass() also requires Judged, so an
	//      unjudged verdict refused at the LAST gate instead.
	//
	// The opt-in below names every class it is allowed to name (A, B1, B2).
	// It cannot name C: this test asserts C is refused even when the operator
	// has opted into everything available, which is the "whatever the opt-in
	// says" the engine comment claims.
	e := &Engine{Applier: refusingApplier{}}
	cfg := config.AssistantConfig{AutoApply: map[string]config.AssistantAutoApply{
		"p": {Classes: []string{ClassA, ClassB1, ClassB2, ClassC, ClassD, ClassE}},
	}}
	pass := JudgeVerdict{Judged: true, Decision: "pass"}

	for _, entrypoint := range []string{
		EntrypointSystem, EntrypointREST, EntrypointCLI, EntrypointConsole,
		EntrypointChat, EntrypointAgent,
	} {
		for _, class := range []string{ClassC, ClassD, ClassE} {
			req := Request{Entrypoint: entrypoint, ProjectID: "p"}
			if e.mayAutoApply(req, cfg, class, pass) {
				t.Errorf("entrypoint %s may auto-apply class %s. Class C is a privilege path "+
					"that never touches a permission key — a topology edit can name a wider role "+
					"and acquire its tools — and no entrypoint may apply it unreviewed",
					entrypoint, class)
			}
		}
	}

	// And the first gate really is only the first: the system entrypoint passes
	// MayAutoApply, which is exactly why the class gate has to be the one that
	// refuses. A test asserting only "it did not auto-apply" would pass against
	// an implementation where MayAutoApply(system) were false, which is a
	// different design (round 3 F5's trap).
	if !MayAutoApply(EntrypointSystem) {
		t.Fatal("MayAutoApply(system) is false, so this test no longer proves the CLASS gate " +
			"is what refuses class C — it would pass for the wrong reason")
	}
}

// refusingApplier is non-nil so mayAutoApply reaches its class gate. It is
// never called: every case under test must be refused before application.
type refusingApplier struct{}

func (refusingApplier) Apply(context.Context, string, string, bool) error {
	panic("the apply path was reached for a class that must never auto-apply")
}
