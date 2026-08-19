package quality

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ScoreKindContractSatisfaction grades the fraction of a workflow's DECLARED
// output obligations that the run actually delivered.
//
// It exists because pinned_case_validation is unreachable on the local
// benchmark model: measured over the ledger, testers that ran emitted
// testing.cases[] 15% of the time on the 27B against 100% on the 397B, so that
// metric floors near zero for reasons unrelated to the release under test.
// "Write artifacts/out/findings.md" is a promise a small model can keep.
//
// WHAT IT MEASURES, AND WHAT IT DOES NOT. It measures that a workflow's
// declared outputs were produced. It does NOT measure whether they are correct,
// complete or useful — a step that writes a valid but substantively empty file
// scores as met. It is a plumbing guardrail that catches "this workflow stopped
// producing its declared output", which is a real and currently undetected
// regression class, and it is a weaker claim than pinned_case_validation. No
// published figure may describe it as a quality score.
//
// See https://docs.vornik.io
const ScoreKindContractSatisfaction ScoreKind = "contract_satisfaction"

// Terminal execution states the scorer distinguishes. Only COMPLETED licenses
// treating an unreached obligation as branch-excluded rather than unmet.
const (
	TerminalCompleted = "COMPLETED"
	TerminalFailed    = "FAILED"
	TerminalCancelled = "CANCELLED"
)

// Contract diagnostics.
const (
	DiagnosticNoDeclaredObligations = "no_declared_obligations"
	DiagnosticCorruptWorkflowShape  = "corrupt_workflow_snapshot"
)

// ContractEvidence is everything the scorer reads. Both fields come from the
// ledger the executor already writes — no new instrumentation, the constraint
// §12.11.5 set for the graded metric and kept here.
type ContractEvidence struct {
	// WorkflowSnapshot is the execution's PINNED workflow definition, so a hot
	// reload cannot reinterpret evidence produced under an older contract
	// (§12.11.6). Empty means the row predates the pin: not_applicable, never
	// a guess from today's registry.
	WorkflowSnapshot []byte
	// StepOutcomes maps step id to the outcome the executor recorded for it.
	//
	// This, not state_snapshot.stepResults, is the evidence source. A snapshot
	// records only steps that SUCCEEDED, so it cannot distinguish a step that
	// ran and failed from one never reached — and those are opposite facts
	// here: the first is a broken promise, the second may be a road not taken.
	// execution_step_outcomes carries a row per attempted step.
	StepOutcomes map[string]string
	// TerminalStatus is the execution's committed terminal state.
	TerminalStatus string
}

// ObligationEvidence is one declared obligation and what became of it. Journaled
// so a score stays auditable after ledger retention expires, the same reason
// §12.11.5 normalizes case evidence.
type ObligationEvidence struct {
	StepID string `json:"stepId"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	// Outcome is what the executor recorded for the step, or "" when the step
	// was never attempted.
	Outcome  string `json:"outcome,omitempty"`
	Met      bool   `json:"met"`
	Excluded bool   `json:"excluded,omitempty"`
}

// workflowShape decodes only what the scorer needs from a pinned snapshot.
//
// The KEYS ARE GO FIELD NAMES. registry.Workflow carries yaml tags and no json
// tags, and the executor pins the snapshot with json.Marshal, so the wire form
// is {"Steps":{"analyze":{"RequireOutputGlob":"..."}}}. Decoding lowercase keys
// would find zero obligations and silently score every execution
// not_applicable — checked against a real ledger row, not assumed.
//
// internal/quality cannot import internal/registry to reuse the type: registry
// already imports this package for ScoringPolicy, so that would be a cycle.
type workflowShape struct {
	Steps map[string]struct {
		RequireOutputGlob string
	}
}

// ScoreContractSatisfaction grades met obligations over declared obligations.
//
// Returns an error only for evidence the scorer cannot READ — a corrupt pinned
// snapshot. That is a harness fault, and charging the measured system a zero
// for it would blame the agent for our own bug; benchmark callers refuse the
// run, while the production publisher maps it to invalid_evidence so the row
// still exists.
func ScoreContractSatisfaction(evidence ContractEvidence) (ExecutionScore, error) {
	if len(evidence.WorkflowSnapshot) == 0 {
		// Retained rows predate the workflow pin. Honestly inapplicable — the
		// publisher never guesses historical applicability from today's YAML.
		return ExecutionScore{Kind: ScoreKindContractSatisfaction, Status: ScoreStatusNotApplicable}, nil
	}
	if !json.Valid(evidence.WorkflowSnapshot) {
		return ExecutionScore{}, fmt.Errorf("decode workflow snapshot: invalid JSON")
	}
	var shape workflowShape
	if err := json.Unmarshal(evidence.WorkflowSnapshot, &shape); err != nil {
		return ExecutionScore{}, fmt.Errorf("decode workflow snapshot: %w", err)
	}

	// Sorted so the journaled evidence body is byte-stable across scorings —
	// Go randomises map iteration, and a flapping body would make two scorings
	// of one execution disagree on their own record.
	stepIDs := make([]string, 0, len(shape.Steps))
	for id := range shape.Steps {
		stepIDs = append(stepIDs, id)
	}
	sort.Strings(stepIDs)

	completed := evidence.TerminalStatus == TerminalCompleted
	result := ExecutionScore{Kind: ScoreKindContractSatisfaction}
	met, unmet := 0, 0
	for _, id := range stepIDs {
		glob := shape.Steps[id].RequireOutputGlob
		if glob == "" {
			continue // declares nothing; contributes to neither side
		}
		outcome, attempted := evidence.StepOutcomes[id]
		item := ObligationEvidence{StepID: id, Kind: "output_glob", Detail: glob, Outcome: outcome}
		switch {
		case attempted && outcome == string(outcomeOK):
			item.Met = true
			met++
		case attempted:
			// Ran and did not succeed: the step that promised an output
			// reached execution and did not deliver it. This is the case the
			// metric exists to catch.
			unmet++
		case completed:
			// Never reached, on a run that finished: the workflow took a
			// different declared branch. Counting it would mean a branchy
			// workflow could never score 1.0 and the ceiling would differ per
			// run, so it leaves the denominator entirely.
			item.Excluded = true
		default:
			// Never reached because the run died, timed out, or was cancelled.
			// Owed: otherwise crashing early would raise a score above a run
			// that finished and missed one.
			unmet++
		}
		result.ObligationEvidence = append(result.ObligationEvidence, item)
	}

	denominator := met + unmet
	if denominator == 0 {
		// Either the workflow declares nothing, or every obligation it declares
		// was branch-excluded. Both are honestly unscorable: the fail-closed
		// rule from §12.11.6 gives an empty state rather than a manufactured
		// 1.0 for a workflow that promised nothing.
		result.Status = ScoreStatusNotApplicable
		result.Diagnostic = DiagnosticNoDeclaredObligations
		return result, nil
	}

	score := float64(met) / float64(denominator)
	result.Status = ScoreStatusScored
	result.Score = &score
	result.PassedCaseCount = met
	result.PinnedCaseCount = denominator
	if unmet > 0 {
		result.Diagnostic = fmt.Sprintf("%d of %d declared obligations unmet", unmet, denominator)
	}
	return result, nil
}

// outcomeOK is the one step outcome that counts as delivering. Declared here
// rather than imported from internal/stepoutcome to keep this package a leaf
// the registry can depend on; the string is asserted against the shared
// constant by a test.
type outcomeLiteral string

const outcomeOK outcomeLiteral = "ok"
