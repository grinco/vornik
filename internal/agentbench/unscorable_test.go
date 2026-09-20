package agentbench

import (
	"math"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

// review-20260919-c82a A2-1: publishing "3 unscorable" beside a mean that
// silently counted those three as 0.000 is the same wrong number wearing a
// correct-looking count. The EXCLUSION is the load-bearing half, so it gets its
// own assertion rather than riding on the print.
//
// taskRepeatScores filtered by score KIND only and never by status, so a
// zero-scored row of any status landed in the mean.

func scoreRow(task string, status quality.ScoreStatus, score float64) TaskScore {
	return TaskScore{
		TaskID: task, Repeat: 1,
		Kind: quality.ScoreKindPinnedCaseValidation, Status: status, Score: score,
	}
}

func TestTaskRepeatScores_ExcludesUnscorableFromTheDenominator(t *testing.T) {
	scores := []TaskScore{
		scoreRow("dp-01", quality.ScoreStatusScored, 1.0),
		scoreRow("dp-02", quality.ScoreStatusScored, 1.0),
		// A rework-loop execution the scorer could not evaluate. Journalled
		// with a zero numeric field, because the journal needs one — which is
		// exactly why the status, not the number, has to decide.
		scoreRow("dp-03", quality.ScoreStatusUnscorable, 0.0),
	}

	got, err := taskRepeatScores(scores, quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["dp-03"]; present {
		t.Fatal("an unscorable task entered the denominator")
	}
	if len(got) != 2 {
		t.Fatalf("want 2 scored tasks, got %d", len(got))
	}

	// The load-bearing comparison: the mean over the scored set must differ
	// from the mean that counts the unscorable row as a zero.
	mean := (got["dp-01"][1] + got["dp-02"][1]) / 2
	asZero := (1.0 + 1.0 + 0.0) / 3
	if math.Abs(mean-asZero) < 1e-9 {
		t.Fatal("excluding the unscorable row made no difference to the mean; it is being counted as zero")
	}
	if mean != 1.0 {
		t.Fatalf("want a mean of 1.000 over the scored set, got %v", mean)
	}
}

// not_applicable is excluded for the same reason and by the same rule: a
// workflow that declares no contract contributes no evidence either way.
func TestTaskRepeatScores_ExcludesNotApplicable(t *testing.T) {
	got, err := taskRepeatScores([]TaskScore{
		scoreRow("dp-01", quality.ScoreStatusScored, 0.5),
		scoreRow("sw-01", quality.ScoreStatusNotApplicable, 0.0),
	}, quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["sw-01"]; present {
		t.Fatal("a not_applicable task entered the denominator")
	}
}

// The statuses that mean the AGENT or the CONTRACT failed keep counting as
// zero. That is what the metric is for, and quietly excluding them would turn
// a fail-closed score into a missing one.
func TestTaskRepeatScores_KeepsInvalidEvidenceAndMissingContractAsZero(t *testing.T) {
	got, err := taskRepeatScores([]TaskScore{
		scoreRow("dp-01", quality.ScoreStatusInvalidEvidence, 0.0),
		scoreRow("dp-02", quality.ScoreStatusMissingContract, 0.0),
	}, quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a fail-closed zero was excluded from the mean: %v", got)
	}
	if got["dp-01"][1] != 0 || got["dp-02"][1] != 0 {
		t.Fatalf("want zeros carried, got %v", got)
	}
}

// The count must be publishable: a mean over a shrunken set is not better than
// a wrong mean unless the reader can see how much fell out.
func TestCountExcluded_ReportsBothReasonsSeparately(t *testing.T) {
	scores := []TaskScore{
		scoreRow("dp-01", quality.ScoreStatusScored, 1.0),
		scoreRow("dp-02", quality.ScoreStatusUnscorable, 0.0),
		scoreRow("dp-03", quality.ScoreStatusUnscorable, 0.0),
		scoreRow("sw-01", quality.ScoreStatusNotApplicable, 0.0),
	}
	summary := SummariseScoreCoverage(scores, quality.ScoreKindPinnedCaseValidation)

	if summary.Scored != 1 {
		t.Fatalf("want 1 scored, got %d", summary.Scored)
	}
	if summary.Unscorable != 2 {
		t.Fatalf("want 2 unscorable, got %d", summary.Unscorable)
	}
	if summary.NotApplicable != 1 {
		t.Fatalf("want 1 not_applicable, got %d", summary.NotApplicable)
	}
	// The two exclusions say different things and must not be merged into one
	// "excluded" figure: one is a workflow declaring no contract, the other is
	// a contract this scorer could not evaluate.
	if summary.ContractDeclaring != 3 {
		t.Fatalf("want 3 contract-declaring tasks, got %d", summary.ContractDeclaring)
	}
}

// The rework-loop tripwire. Every scored task in the 2026.9.4 arm ran ONE
// implement and ONE test, so the loop dev-pipeline is built around — the
// failing-test path, the reviewer-rejection path, the checkpoint/resume branch
// the autonomy loop depends on — was unmeasured on every task and every repeat,
// and nothing in the journal or the scoreboard said so.

func visitRow(task string, verifier int) TaskScore {
	s := scoreRow(task, quality.ScoreStatusScored, 1.0)
	s.ProducerVisits, s.VerifierVisits = 1, verifier
	return s
}

func TestReworkCoverage_StraightLineArmSaysSo(t *testing.T) {
	c := SummariseScoreCoverage([]TaskScore{
		visitRow("dp-01", 1),
		visitRow("dp-02", 1),
	}, quality.ScoreKindPinnedCaseValidation)

	if c.ReworkExercised != 0 || c.VisitsKnown != 2 {
		t.Fatalf("want 0 of 2 exercised, got %d of %d", c.ReworkExercised, c.VisitsKnown)
	}
	got := c.ReworkCoverage()
	if !strings.Contains(got, "NOT EXERCISED") || !strings.Contains(got, "0 of 2") {
		t.Fatalf("a straight-line arm must say so with its numbers: %q", got)
	}
}

func TestReworkCoverage_LoopingArmIsReported(t *testing.T) {
	c := SummariseScoreCoverage([]TaskScore{
		visitRow("dp-01", 3),
		visitRow("dp-02", 1),
	}, quality.ScoreKindPinnedCaseValidation)

	if c.ReworkExercised != 1 {
		t.Fatalf("want 1 exercised, got %d", c.ReworkExercised)
	}
	if got := c.ReworkCoverage(); !strings.Contains(got, "exercised by 1 of 2") {
		t.Fatalf("unexpected: %q", got)
	}
}

// A journal written before visit counts existed reports zero for a reason that
// has nothing to do with the arm. "Not measured" must not read as "measured,
// and it never looped" — that is the same cannot-distinguish failure this
// tripwire exists to remove.
func TestReworkCoverage_MissingVisitCountsIsNotAZero(t *testing.T) {
	c := SummariseScoreCoverage([]TaskScore{
		scoreRow("dp-01", quality.ScoreStatusScored, 1.0),
	}, quality.ScoreKindPinnedCaseValidation)

	if c.VisitsKnown != 0 {
		t.Fatalf("want no visit counts known, got %d", c.VisitsKnown)
	}
	if got := c.ReworkCoverage(); !strings.Contains(got, "NOT MEASURED") {
		t.Fatalf("an old journal must report not-measured, got %q", got)
	}
}
