package registry

import (
	"strings"
	"testing"
)

// These tests pin the semantic-validation rules in Workflow.Validate that
// stop a structurally-broken workflow from ever reaching the executor.
// They construct Workflow structs directly (rather than round-tripping
// through LoadWorkflows) so each rule is exercised in isolation and the
// returned WorkflowValidationError's Field/Message can be asserted — the
// operator-facing contract for "why was my config rejected".
//
// Coverage focus: the step-type-specific required-field branches, terminal
// status validity, transition target resolution (step vs terminal vs
// dangling), reachability, the description cap, and the
// ingest_input_artifacts empty-steps exception. The agent-step
// gates+on_success footgun guard lives in workflow_gate_onsuccess_test.go
// and is deliberately not duplicated here.

// helper: a minimal valid agent workflow we can mutate per-case.
func validAgentWorkflow() *Workflow {
	return &Workflow{
		ID:         "wf",
		Entrypoint: "start",
		Steps: map[string]WorkflowStep{
			"start": {Type: "agent", Role: "coder", OnSuccess: "done"},
		},
		Terminals: map[string]WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
	}
}

func TestValidate_ValidAgentWorkflow_Accepts(t *testing.T) {
	if err := validAgentWorkflow().Validate("wf.md"); err != nil {
		t.Fatalf("baseline valid workflow rejected: %v", err)
	}
}

// --- step type allowlist ---

func TestValidate_UnknownStepType_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "wizardry", OnSuccess: "done"}
	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("expected rejection of unknown step type")
	}
	if !strings.Contains(err.Error(), "invalid step type") {
		t.Errorf("error should explain invalid step type, got: %v", err)
	}
}

func TestValidate_AllValidStepTypes_Accepted(t *testing.T) {
	// Each non-agent type wired with its required companion fields so we
	// exercise the type allowlist for every entry, not just "agent".
	cases := map[string]WorkflowStep{
		"gate":          {Type: "gate", Gates: []WorkflowGate{{Condition: "x == true", Target: "done"}}},
		"approval":      {Type: "approval", OnSuccess: "done"},
		"plan":          {Type: "plan", Role: "lead", OnSuccess: "done"},
		"system":        {Type: "system", Handler: "rag.index", OnSuccess: "done"},
		"spawn_project": {Type: "spawn_project", Template: "sales-campaign", OnSuccess: "done"},
		"a2a_call":      {Type: "a2a_call", AgentURL: "https://x/a2a", OnSuccess: "done"},
		"call_project": {
			Type: "call_project", TargetProject: "p", TargetWorkflow: "w",
			Expect: WorkflowCallExpect{Schema: "envelope.v1"}, OnSuccess: "done",
		},
	}
	for typ, step := range cases {
		t.Run(typ, func(t *testing.T) {
			w := &Workflow{
				ID:         "wf",
				Entrypoint: "start",
				Steps:      map[string]WorkflowStep{"start": step},
				Terminals:  map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
			}
			if err := w.Validate("wf.md"); err != nil {
				t.Fatalf("step type %q with required fields should validate, got: %v", typ, err)
			}
		})
	}
}

// --- per-type required fields ---

func TestValidate_PlanStepRequiresRole(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "plan", OnSuccess: "done"} // no role
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "role is required") {
		t.Fatalf("plan step without role should be rejected, got: %v", err)
	}
}

func TestValidate_PlanStepRequiresOnSuccess(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "plan", Role: "lead"} // no on_success
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "on_success is required") {
		t.Fatalf("plan step without on_success should be rejected, got: %v", err)
	}
}

func TestValidate_SystemStepRequiresHandler(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "system", OnSuccess: "done"} // no handler
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "handler is required") {
		t.Fatalf("system step without handler should be rejected, got: %v", err)
	}
	if verr, ok := err.(WorkflowValidationError); ok && !strings.Contains(verr.Field, "handler") {
		t.Errorf("field should reference handler, got %q", verr.Field)
	}
}

func TestValidate_SpawnProjectStepRequiresTemplate(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "spawn_project", OnSuccess: "done"} // no template
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "template is required") {
		t.Fatalf("spawn_project step without template should be rejected, got: %v", err)
	}
}

func TestValidate_CallProjectStep_RequiredFields(t *testing.T) {
	base := func() WorkflowStep {
		return WorkflowStep{
			Type: "call_project", TargetProject: "p", TargetWorkflow: "w",
			Expect: WorkflowCallExpect{Schema: "envelope.v1"}, OnSuccess: "done",
		}
	}
	missing := map[string]func(WorkflowStep) WorkflowStep{
		"target_project":  func(s WorkflowStep) WorkflowStep { s.TargetProject = ""; return s },
		"target_workflow": func(s WorkflowStep) WorkflowStep { s.TargetWorkflow = ""; return s },
		"expect.schema":   func(s WorkflowStep) WorkflowStep { s.Expect.Schema = ""; return s },
	}
	for field, mutate := range missing {
		t.Run("missing_"+field, func(t *testing.T) {
			w := validAgentWorkflow()
			w.Steps["start"] = mutate(base())
			err := w.Validate("wf.md")
			if err == nil {
				t.Fatalf("call_project without %s should be rejected", field)
			}
			verr, ok := err.(WorkflowValidationError)
			if !ok || !strings.Contains(verr.Field, field) {
				t.Errorf("error field should reference %q, got: %v", field, err)
			}
		})
	}
}

// --- transition resolution ---

func TestValidate_OnSuccessToTerminal_Accepts(t *testing.T) {
	// on_success pointing straight at a terminal is the common happy path.
	if err := validAgentWorkflow().Validate("wf.md"); err != nil {
		t.Fatalf("on_success -> terminal should validate, got: %v", err)
	}
}

func TestValidate_OnSuccessToDanglingTarget_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{Type: "agent", Role: "coder", OnSuccess: "ghost"}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "not found in steps or terminals") {
		t.Fatalf("on_success to dangling target should be rejected, got: %v", err)
	}
}

func TestValidate_GateTargetToDanglingTarget_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{
		Type: "agent", Role: "reviewer", OnFail: "done",
		Gates: []WorkflowGate{{Condition: "ok == true", Target: "ghost"}},
	}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "not found in steps or terminals") {
		t.Fatalf("gate target to dangling step should be rejected, got: %v", err)
	}
}

func TestValidate_GateMissingCondition_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{
		Type: "agent", Role: "reviewer", OnFail: "done",
		Gates: []WorkflowGate{{Target: "done"}}, // no condition
	}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "gate condition is required") {
		t.Fatalf("gate without condition should be rejected, got: %v", err)
	}
}

func TestValidate_GateMissingTarget_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Steps["start"] = WorkflowStep{
		Type: "agent", Role: "reviewer", OnFail: "done",
		Gates: []WorkflowGate{{Condition: "ok == true"}}, // no target
	}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "gate target is required") {
		t.Fatalf("gate without target should be rejected, got: %v", err)
	}
}

// --- terminals ---

func TestValidate_InvalidTerminalStatus_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Terminals["done"] = WorkflowTerminal{Status: "DEFENESTRATED"}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "invalid terminal status") {
		t.Fatalf("bogus terminal status should be rejected, got: %v", err)
	}
}

func TestValidate_TerminalMissingStatus_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Terminals["done"] = WorkflowTerminal{} // empty status
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "terminal status is required") {
		t.Fatalf("terminal without status should be rejected, got: %v", err)
	}
}

func TestValidate_AllTerminalStatuses_Accepted(t *testing.T) {
	for _, status := range []string{"COMPLETED", "FAILED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			w := validAgentWorkflow()
			w.Terminals["done"] = WorkflowTerminal{Status: status}
			if err := w.Validate("wf.md"); err != nil {
				t.Fatalf("terminal status %q should be valid, got: %v", status, err)
			}
		})
	}
}

// --- reachability ---

func TestValidate_UnreachableStep_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	// "orphan" is never referenced by any transition from the entrypoint.
	w.Steps["orphan"] = WorkflowStep{Type: "agent", Role: "coder", OnSuccess: "done"}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "not reachable from entrypoint") {
		t.Fatalf("unreachable step should be rejected, got: %v", err)
	}
	if verr, ok := err.(WorkflowValidationError); ok && !strings.Contains(verr.Field, "orphan") {
		t.Errorf("field should name the unreachable step, got %q", verr.Field)
	}
}

func TestValidate_StepReachableViaGateAndOnFail_Accepts(t *testing.T) {
	// Reachability must follow gate targets and on_fail, not just on_success.
	w := &Workflow{
		ID:         "wf",
		Entrypoint: "review",
		Steps: map[string]WorkflowStep{
			"review": {
				Type: "agent", Role: "reviewer", OnFail: "rescue",
				Gates: []WorkflowGate{{Condition: "ok == true", Target: "publish"}},
			},
			"publish": {Type: "agent", Role: "pub", OnSuccess: "done"},   // via gate
			"rescue":  {Type: "agent", Role: "fixer", OnSuccess: "done"}, // via on_fail
		},
		Terminals: map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("steps reachable via gate/on_fail should validate, got: %v", err)
	}
}

// --- description cap ---

func TestValidate_DescriptionWithinCap_Accepts(t *testing.T) {
	w := validAgentWorkflow()
	w.Description = strings.Repeat("x", WorkflowDescriptionMaxLen)
	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("description at exactly the cap should validate, got: %v", err)
	}
}

func TestValidate_DescriptionOverCap_Rejected(t *testing.T) {
	w := validAgentWorkflow()
	w.Description = strings.Repeat("x", WorkflowDescriptionMaxLen+1)
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "description must be") {
		t.Fatalf("over-cap description should be rejected, got: %v", err)
	}
}

// --- ingest_input_artifacts empty-steps exception ---

func TestValidate_EmptyStepsRejectedWithoutIngestFlag(t *testing.T) {
	w := &Workflow{
		ID:         "wf",
		Entrypoint: "done",
		Terminals:  map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	err := w.Validate("wf.md")
	if err == nil || !strings.Contains(err.Error(), "at least one step is required") {
		t.Fatalf("empty steps without ingest flag should be rejected, got: %v", err)
	}
}

func TestValidate_EmptyStepsAllowedForIngestWorkflow(t *testing.T) {
	// The deterministic ingest workflow has no agent step: the entrypoint
	// routes straight to a terminal and handleSuccess does the ingest.
	w := &Workflow{
		ID:                   "ingest",
		Entrypoint:           "done",
		IngestInputArtifacts: true,
		Terminals:            map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	if err := w.Validate("ingest.md"); err != nil {
		t.Fatalf("ingest workflow with entrypoint->terminal should validate, got: %v", err)
	}
}
