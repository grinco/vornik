package agentbench

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

// Regression, measured 2026-08-18 on the benchmark host.
//
// The tripwire tier decided pass/fail on TASK STATUS alone. dev-pipeline routes
// implement/test/review on_fail to recover-checkpoint, which parks the work and
// exits via a COMPLETED terminal, so the task reads green while the step that
// verifies anything failed every attempt. Three repeats scored 3/3 TRIPWIRE OK
// with, in the very same runs:
//
//	missing_contract  pinned=7  missing_verifier_step  x2
//	invalid_evidence  pinned=0  no_pinned_cases        x1
//
// A tier whose job is "did we break this workflow outright" cannot answer that
// from a status a recovery path can manufacture. Where the workflow declares a
// contract, the contract is the honest signal.
func TestTripwireDecision_SucceededTaskWithUnmetContractIsNotAPass(t *testing.T) {
	tripwires := map[string]bool{"tw-pipeline": true}
	// Exactly tonight's shape: the task reached a COMPLETED terminal, and the
	// scorer recorded that the declared contract went unmet.
	journal := Journal{
		TaskRuns: []TaskRun{{
			TaskID: "tw-pipeline", Repeat: 1, Succeeded: true,
			ExecutionIDs: []string{"exec-1"},
		}},
		TaskScores: []TaskScore{{
			TaskID: "tw-pipeline", Repeat: 1,
			Kind:            quality.ScoreKindPinnedCaseValidation,
			Status:          quality.ScoreStatusMissingContract,
			Score:           0,
			PinnedCaseCount: 7,
			Diagnostic:      quality.DiagnosticMissingVerifierStep,
		}},
	}

	got := releaseTripwireDecision(journal, journal, tripwires)
	if got == nil {
		t.Fatal("a tripwire whose declared contract went unmet must not pass — task status " +
			"can be manufactured by a recovery path, which is exactly how dev-pipeline " +
			"scored 3/3 with its verifier failing every attempt")
	}
	if !strings.Contains(got.Reason, "tw-pipeline") {
		t.Errorf("the refusal must name the tripwire, got %q", got.Reason)
	}
}

// A tripwire whose workflow declares NO contract has nothing else to assert, so
// task status remains the signal. Without this the change would fail every
// tripwire on a workflow that declares no obligations — which is most of them.
func TestTripwireDecision_NoContractFallsBackToTaskStatus(t *testing.T) {
	tripwires := map[string]bool{"tw-simple": true}
	journal := Journal{
		TaskRuns: []TaskRun{{TaskID: "tw-simple", Repeat: 1, Succeeded: true}},
		// not_applicable: the workflow declares nothing to verify.
		TaskScores: []TaskScore{{
			TaskID: "tw-simple", Repeat: 1,
			Status: quality.ScoreStatusNotApplicable,
		}},
	}
	if got := releaseTripwireDecision(journal, journal, tripwires); got != nil {
		t.Errorf("a contract-less tripwire that succeeded must pass, got refusal %q", got.Reason)
	}
}

// A fully met contract passes — the change must not turn a healthy tripwire red.
func TestTripwireDecision_MetContractPasses(t *testing.T) {
	tripwires := map[string]bool{"tw-pipeline": true}
	journal := Journal{
		TaskRuns: []TaskRun{{TaskID: "tw-pipeline", Repeat: 1, Succeeded: true}},
		TaskScores: []TaskScore{{
			TaskID: "tw-pipeline", Repeat: 1,
			Kind: quality.ScoreKindPinnedCaseValidation, Status: quality.ScoreStatusScored,
			Score: 1, PassedCaseCount: 7, PinnedCaseCount: 7,
		}},
	}
	if got := releaseTripwireDecision(journal, journal, tripwires); got != nil {
		t.Errorf("a met contract must pass, got refusal %q", got.Reason)
	}
}

// A partially met contract is still a broken tripwire: the tier is binary by
// design (validateCalibrationTier demands passed == attempts), so "most of the
// contract" is not a pass.
func TestTripwireDecision_PartialContractIsNotAPass(t *testing.T) {
	tripwires := map[string]bool{"tw-pipeline": true}
	journal := Journal{
		TaskRuns: []TaskRun{{TaskID: "tw-pipeline", Repeat: 1, Succeeded: true}},
		TaskScores: []TaskScore{{
			TaskID: "tw-pipeline", Repeat: 1,
			Kind: quality.ScoreKindPinnedCaseValidation, Status: quality.ScoreStatusScored,
			Score: 0.5, PassedCaseCount: 1, PinnedCaseCount: 2,
		}},
	}
	if got := releaseTripwireDecision(journal, journal, tripwires); got == nil {
		t.Error("a partially met contract must not pass a binary tier")
	}
}
