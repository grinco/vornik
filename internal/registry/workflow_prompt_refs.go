package registry

import (
	"fmt"
	"regexp"

	"vornik.io/vornik/internal/quality"
)

// promptOutputRefRegex matches ${outputs.<step-id>.<field-path>} inside a step
// prompt. Kept in step with outputRefRegex in internal/executor — the executor
// owns resolution, this package owns the load-time contract, and a reference
// this does not recognise is one the executor will not resolve either.
var promptOutputRefRegex = regexp.MustCompile(`\$\{outputs\.([A-Za-z0-9_\-]+)\.([A-Za-z0-9_.\-]+)\}`)

// validatePromptOutputRefs enforces the two load-time rules for
// ${outputs.<step>.<field>} references in agent step prompts
// (2026-08-13-agent-quality-benchmark-design.md, amendment 2026-09-18, D1a/D2).
//
//  1. The referenced step must exist. A typo is indistinguishable at run time
//     from a step that legitimately has not run yet, so it must never reach a
//     run.
//
//  2. A SCORING VERIFIER's prompt may not reference a step reachable from
//     itself. The executor interpolates from LIVE state at step-prep time
//     while the scorer reads the PERSISTED snapshot at scoring time; those are
//     the same bytes only while the referenced step cannot re-run in between.
//     reachableFrom walks on_success, on_fail AND gate targets (see
//     stepSuccessors), so this catches a gate-driven rework loop — which is
//     the shape dev-pipeline actually has (`testing.passed == false` →
//     implement → test), not merely an on_fail cycle.
func (w *Workflow) validatePromptOutputRefs(filename string) error {
	verifierStep := ""
	if p := w.QualityScoring; p != nil && p.Kind == quality.ScoreKindPinnedCaseValidation {
		verifierStep = p.VerifierStep
	}

	// Steps reachable from the scoring verifier, computed once. Empty when the
	// workflow declares no pinned-case scoring.
	var reachableFromVerifier map[string]bool
	if verifierStep != "" {
		reachableFromVerifier = make(map[string]bool)
		if step, ok := w.Steps[verifierStep]; ok {
			for _, next := range w.stepSuccessors(step) {
				w.reachableFrom(next, reachableFromVerifier)
			}
		}
	}

	for _, stepID := range sortedStepIDs(w.Steps) {
		step := w.Steps[stepID]
		for _, m := range promptOutputRefRegex.FindAllStringSubmatch(step.Prompt, -1) {
			ref, refStep, refField := m[0], m[1], m[2]
			if _, ok := w.Steps[refStep]; !ok {
				return WorkflowValidationError{
					File:  filename,
					Field: fmt.Sprintf("steps.%s.prompt", stepID),
					Message: fmt.Sprintf(
						"%s references step %q, which this workflow does not define",
						ref, refStep),
				}
			}
			if stepID == verifierStep && reachableFromVerifier[refStep] {
				return WorkflowValidationError{
					File:  filename,
					Field: fmt.Sprintf("steps.%s.prompt", stepID),
					Message: fmt.Sprintf(
						"%s references step %q, which is reachable from the scoring verifier %q: "+
							"the producer could re-run after the verifier was handed its value, so the "+
							"interpolated value and the value the scorer reads from the snapshot can diverge "+
							"(field %q)",
						ref, refStep, verifierStep, refField),
				}
			}
		}
	}
	return nil
}
