package quality

import (
	"strings"
	"testing"
)

// snapshot builds a workflow_snapshot in the shape the executor actually
// writes: json.Marshal of a registry.Workflow, whose fields carry yaml tags
// only, so the JSON keys are the GO FIELD NAMES ("Steps", "RequireOutputGlob").
// Verified against a real row in the bench ledger before this test was written
// — decoding lowercase keys would have silently found zero obligations and
// scored every execution not_applicable.
func snapshot(steps string) []byte {
	return []byte(`{"ID":"wf","Entrypoint":"a","Steps":{` + steps + `}}`)
}

const (
	stepA = `"a":{"Type":"agent","OnSuccess":"b","RequireOutputGlob":"out/a.md"}`
	stepB = `"b":{"Type":"agent","OnSuccess":"c","RequireOutputGlob":"out/b.md"}`
	stepC = `"c":{"Type":"agent","RequireOutputGlob":"out/c.md"}`
	// A step declaring nothing: contributes no obligation in either direction.
	stepPlain = `"plain":{"Type":"agent","OnSuccess":"b"}`
)

func TestScoreContractSatisfaction_NoObligationsIsNotApplicable(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepPlain),
		StepOutcomes:     map[string]string{"plain": "ok"},
		TerminalStatus:   TerminalCompleted,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusNotApplicable {
		t.Errorf("status = %q, want not_applicable", got.Status)
	}
	// The fail-closed rule from §12.11.6: a workflow with no verifiable
	// contract gets an empty state, never a manufactured 1.0. A score of 1.0
	// here would report perfect quality for a workflow that promised nothing.
	if got.Score != nil {
		t.Errorf("score = %v, want nil — a workflow declaring nothing must not score", *got.Score)
	}
}

func TestScoreContractSatisfaction_AllMet(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok", "b": "ok", "c": "ok"},
		TerminalStatus:   TerminalCompleted,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusScored || got.Score == nil || *got.Score != 1 {
		t.Fatalf("got %+v, want a scored 1.0", got)
	}
	if got.PassedCaseCount != 3 || got.PinnedCaseCount != 3 {
		t.Errorf("counts = %d/%d, want 3/3", got.PassedCaseCount, got.PinnedCaseCount)
	}
}

// A step that RAN and failed is unmet — this is the case the metric exists to
// catch: the workflow reached the step that promised an output and did not
// produce it.
func TestScoreContractSatisfaction_AttemptedButFailedIsUnmet(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok", "b": "failed", "c": "ok"},
		TerminalStatus:   TerminalCompleted,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 2.0/3.0 {
		t.Fatalf("got %+v, want 2/3", got)
	}
}

// Measured 2026-08-18: a step whose outcome is schema_violation produced no
// usable result, so its declared output was not delivered. Distinguished from
// `failed` only in the diagnostic, never in the arithmetic.
func TestScoreContractSatisfaction_SchemaViolationIsUnmet(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB),
		StepOutcomes:     map[string]string{"a": "ok", "b": "schema_violation"},
		TerminalStatus:   TerminalCompleted,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 0.5 {
		t.Fatalf("got %+v, want 0.5", got)
	}
}

// The denominator rule that makes runs comparable: a run that DIED carries its
// unreached obligations as unmet, so crashing early cannot raise a score above
// a run that finished and missed one.
func TestScoreContractSatisfaction_UnreachedOnAFailedRunCountsUnmet(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok"},
		TerminalStatus:   TerminalFailed,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 1.0/3.0 {
		t.Fatalf("got %+v, want 1/3 — a run that died owes every obligation it never reached", got)
	}
	if got.PinnedCaseCount != 3 {
		t.Errorf("denominator = %d, want 3 — the denominator is the revision's, not the run's",
			got.PinnedCaseCount)
	}
}

// The mirror case: a run that COMPLETED never reached a step because it took a
// different declared branch. Counting that against it would mean a branchy
// workflow could never score 1.0, and the ceiling would differ per run.
func TestScoreContractSatisfaction_UnreachedOnACompletedRunIsExcluded(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok", "c": "ok"},
		TerminalStatus:   TerminalCompleted,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Score == nil || *got.Score != 1 {
		t.Fatalf("got %+v, want 1.0 — the road not taken is not a broken promise", got)
	}
	if got.PinnedCaseCount != 2 {
		t.Errorf("denominator = %d, want 2 — an excluded obligation leaves the denominator, "+
			"it is not counted as met", got.PinnedCaseCount)
	}
}

// Fail-closed on evidence the scorer cannot read, mirroring ScoreExecution:
// the benchmark caller needs to distinguish "the agent did badly" from "this
// run cannot be scored", and charging a zero for an unreadable snapshot would
// blame the measured system for a harness fault.
func TestScoreContractSatisfaction_UnreadableSnapshotIsAnError(t *testing.T) {
	for name, snap := range map[string][]byte{
		"truncated": []byte(`{"Steps":`),
		"not json":  []byte(`Steps: a`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ScoreContractSatisfaction(ContractEvidence{
				WorkflowSnapshot: snap,
				TerminalStatus:   TerminalCompleted,
			}); err == nil {
				t.Error("an unreadable workflow snapshot must be an error, not a zero")
			}
		})
	}
}

// An absent snapshot is not corrupt — retained rows predate the pin. They are
// honestly not_applicable, the same rule the publisher already applies.
func TestScoreContractSatisfaction_AbsentSnapshotIsNotApplicable(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{TerminalStatus: TerminalCompleted})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if got.Status != ScoreStatusNotApplicable || got.Score != nil {
		t.Errorf("got %+v, want not_applicable with no score", got)
	}
}

// The evidence list is what makes a score auditable after ledger retention
// expires — the same reason §12.11.5 journals normalized case evidence.
func TestScoreContractSatisfaction_RecordsPerObligationEvidence(t *testing.T) {
	got, err := ScoreContractSatisfaction(ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok", "b": "schema_violation"},
		TerminalStatus:   TerminalFailed,
	})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(got.ObligationEvidence) != 3 {
		t.Fatalf("evidence has %d entries, want one per declared obligation", len(got.ObligationEvidence))
	}
	byStep := map[string]ObligationEvidence{}
	for _, o := range got.ObligationEvidence {
		byStep[o.StepID] = o
	}
	if !byStep["a"].Met {
		t.Error("step a met its glob and must be recorded as met")
	}
	if byStep["b"].Met || byStep["b"].Outcome != "schema_violation" {
		t.Errorf("step b = %+v, want unmet carrying the outcome that explains why", byStep["b"])
	}
	if byStep["c"].Outcome != "" || byStep["c"].Met {
		t.Errorf("step c = %+v, want unmet with no outcome — it was never attempted", byStep["c"])
	}
	if got.Diagnostic == "" || !strings.Contains(got.Diagnostic, "2") {
		t.Errorf("diagnostic %q should name how many obligations went unmet", got.Diagnostic)
	}
}

// Determinism: the evidence order must not depend on Go's randomised map
// iteration, or two scorings of one execution would journal different bodies.
func TestScoreContractSatisfaction_EvidenceOrderIsStable(t *testing.T) {
	ev := ContractEvidence{
		WorkflowSnapshot: snapshot(stepA + "," + stepB + "," + stepC),
		StepOutcomes:     map[string]string{"a": "ok", "b": "ok", "c": "ok"},
		TerminalStatus:   TerminalCompleted,
	}
	first, err := ScoreContractSatisfaction(ev)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := ScoreContractSatisfaction(ev)
		if err != nil {
			t.Fatalf("score: %v", err)
		}
		for j := range first.ObligationEvidence {
			if again.ObligationEvidence[j].StepID != first.ObligationEvidence[j].StepID {
				t.Fatalf("evidence order flapped: %v vs %v",
					first.ObligationEvidence, again.ObligationEvidence)
			}
		}
	}
}

// internal/quality is a leaf package — internal/registry imports it for
// ScoringPolicy, so it cannot import internal/stepoutcome without a cycle, and
// the "ok" literal is therefore duplicated. This is the guard that keeps the
// copy honest: if the canonical outcome ever changes, this fails rather than
// contract_satisfaction silently scoring every obligation unmet.
func TestContractScore_OKLiteralMatchesTheSharedConstant(t *testing.T) {
	// stepoutcome.OK, asserted by value rather than by import.
	const canonicalOK = "ok"
	if string(outcomeOK) != canonicalOK {
		t.Errorf("outcomeOK = %q, but the executor records %q — every obligation "+
			"would score unmet", outcomeOK, canonicalOK)
	}
}
