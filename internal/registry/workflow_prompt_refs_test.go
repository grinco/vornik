package registry

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

// These pin D1a and D2's load-time half (agent-quality-benchmark design,
// amendment 2026-09-18). A ${outputs.<step>.<field>} reference in an agent
// step prompt is the channel that hands a scoring verifier the exact id list
// the scorer will divide by. Two ways to get it wrong are catchable before a
// run starts, and both must be, because the runtime symptom — a verifier
// handed the wrong list — is scored rather than raised.

func promptRefWorkflow(t *testing.T, testPrompt string, producerLoops bool) *Workflow {
	t.Helper()
	analyzeOnSuccess := "implement"
	w := &Workflow{
		ID:         "wf-promptrefs",
		Entrypoint: "analyze",
		Steps: map[string]WorkflowStep{
			"analyze":   {Type: "agent", Role: "analyst", OnSuccess: analyzeOnSuccess},
			"implement": {Type: "agent", Role: "coder", OnSuccess: "test"},
			"test": {
				Type:   "agent",
				Role:   "tester",
				Prompt: testPrompt,
				Gates: []WorkflowGate{
					{Condition: "testing.passed == true", Target: "done"},
					{Condition: "testing.passed == false", Target: "implement"},
				},
			},
		},
		Terminals: map[string]WorkflowTerminal{
			"done": {Status: "COMPLETED"},
		},
		QualityScoring: &quality.ScoringPolicy{
			Kind:         quality.ScoreKindPinnedCaseValidation,
			ProducerStep: "analyze",
			VerifierStep: "test",
		},
	}
	if producerLoops {
		// Put the producer back inside the verifier's own rework loop, which
		// is the shape D1a forbids: `analyze` can re-run after `test` has
		// begun, so the value interpolated into the tester's prompt at
		// prep time need not be the value the scorer reads from the
		// snapshot at scoring time.
		w.Steps["test"] = withGateTarget(w.Steps["test"], "testing.passed == false", "analyze")
	}
	return w
}

func withGateTarget(s WorkflowStep, condition, target string) WorkflowStep {
	for i, g := range s.Gates {
		if g.Condition == condition {
			s.Gates[i].Target = target
		}
	}
	return s
}

// Test 42: a prompt reference to a step the workflow does not contain is a
// typo, and a typo must never reach a run — at runtime it is indistinguishable
// from a step that legitimately has not run yet.
func TestValidate_PromptRefToUnknownStepIsRejected(t *testing.T) {
	// Stands in for a fat-fingered step name. Spelled so it is unmistakably
	// not a real step rather than as a near-miss of "analyze", which the
	// misspell linter would flag in the fixture itself.
	w := promptRefWorkflow(t, "ids: ${outputs.analyze_step.analysis.test_case_ids}", false)

	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("prompt reference to a nonexistent step loaded cleanly; want a validation error")
	}
	if !strings.Contains(err.Error(), "analyze_step") {
		t.Errorf("error does not name the offending step: %v", err)
	}
}

// Test 48 (D1a): the producer step a scoring verifier interpolates from must
// not be reachable from that verifier, or the interpolated value and the
// scored value can diverge. Raised as F1 of review-20260918-384e.
func TestValidate_ScoringProducerReachableFromVerifierIsRejected(t *testing.T) {
	w := promptRefWorkflow(t, "ids: ${outputs.analyze.analysis.test_case_ids}", true)

	err := w.Validate("wf.md")
	if err == nil {
		t.Fatal("re-entrant producer loaded cleanly; want a validation error")
	}
	if !strings.Contains(err.Error(), "analyze") {
		t.Errorf("error does not name the producer step: %v", err)
	}
}

// The same workflow with the producer OUTSIDE the verifier's loop is the
// shipped dev-pipeline shape and must keep loading — the guard is not allowed
// to reject the only configuration that actually works.
func TestValidate_PromptRefToNonReentrantProducerIsAccepted(t *testing.T) {
	w := promptRefWorkflow(t, "ids: ${outputs.analyze.analysis.test_case_ids}", false)

	if err := w.Validate("wf.md"); err != nil {
		t.Fatalf("valid producer reference rejected: %v", err)
	}
}
