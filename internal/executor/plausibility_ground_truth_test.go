package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression, measured on bench 2026-08-18/19: the assistant-swarm planner
// failed its `written_implies_path` plausibility rule in 62 of 76 attempts
// (82%), emitting {"planning":{"written":true},"produced_files":[]} — an empty
// self-report — while the plan file it was supposed to produce demonstrably
// existed. Only 2 of 21 first-attempt failures were rescued by the three-rung
// retry ladder; the rest burned every rung and still failed.
//
// The step already declares require_output_glob: "artifacts/out/plan.md", and
// that check runs FIRST in runContainerStep, verifying against the real
// filesystem. It passed. Twenty lines later the step was failed anyway because
// the model had not remembered to name the same file in produced_files.
//
// Two checks assert the same proposition — "this step produced its declared
// output" — one against reality and one against the model's memory of reality.
// When the ground-truth check has already confirmed it, the model's bookkeeping
// is not grounds to fail the step.
//
// This does NOT weaken the anti-hallucination direction, which is the opposite
// case: a model CLAIMING a file it did not write is still caught, by
// verifyClaimedFiles (claims → filesystem) and by the glob check itself when
// nothing was written.
func TestEvaluatePlausibility_GlobSatisfiedDemotesProducedFilesViolation(t *testing.T) {
	// Exactly what the planner emitted on a rejected attempt.
	payload := []byte(`{"planning": {"written": true}, "produced_files": []}`)
	rules := []registry.PlausibilityRule{{
		Name:    "written_implies_path",
		When:    map[string]any{"planning.written": true},
		Require: []string{"produced_files"},
	}}

	t.Run("glob satisfied: advisory, step survives", func(t *testing.T) {
		got := EvaluatePlausibilityWithGroundTruth(payload, rules, true)
		if len(got) != 1 {
			t.Fatalf("expected the violation to still be REPORTED (the bookkeeping gap "+
				"stays visible), got %d", len(got))
		}
		if !got[0].WarnOnly {
			t.Error("a produced_files violation must be advisory when require_output_glob " +
				"already verified the file against the filesystem — otherwise a weaker " +
				"self-report check overrides a stronger ground-truth one that just passed")
		}
	})

	t.Run("no glob: still gates", func(t *testing.T) {
		got := EvaluatePlausibilityWithGroundTruth(payload, rules, false)
		if len(got) != 1 || got[0].WarnOnly {
			t.Errorf("without a satisfied glob there is no ground truth to defer to, so "+
				"the rule must still gate; got %+v", got)
		}
	})
}

// The demotion is scoped to the field the glob actually speaks to. A rule
// requiring an explanatory field asserts something the filesystem cannot
// confirm, so a satisfied glob is irrelevant to it and it must keep gating.
func TestEvaluatePlausibility_GlobDoesNotDemoteUnrelatedFields(t *testing.T) {
	payload := []byte(`{"planning": {"written": false}, "produced_files": []}`)
	rules := []registry.PlausibilityRule{{
		Name:    "not_written_implies_reason",
		When:    map[string]any{"planning.written": false},
		Require: []string{"planning.reason"},
	}}
	got := EvaluatePlausibilityWithGroundTruth(payload, rules, true)
	if len(got) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(got))
	}
	if got[0].WarnOnly {
		t.Error("planning.reason is not a file-production claim — a satisfied output glob " +
			"says nothing about whether the model explained itself, so this must still gate")
	}
}

// The existing entry point must keep gating unconditionally, so callers that
// have no ground truth to offer are unaffected.
func TestEvaluatePlausibility_UnchangedWithoutGroundTruth(t *testing.T) {
	payload := []byte(`{"planning": {"written": true}, "produced_files": []}`)
	rules := []registry.PlausibilityRule{{
		Name:    "written_implies_path",
		When:    map[string]any{"planning.written": true},
		Require: []string{"produced_files"},
	}}
	got := EvaluatePlausibility(payload, rules)
	if len(got) != 1 || got[0].WarnOnly {
		t.Errorf("EvaluatePlausibility must be unchanged: %+v", got)
	}
}
