package cli

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/quality"
)

// The design's other half (06-executor.md 2026-09-19, review-20260919-c82a
// A2-1/A2-3): excluding unscorable rows from the denominator is only honest if
// the reader can see how many fell out and why. A mean printed alone over a
// shrunken set is the same silent wrong number in a cleaner status.

func row(task string, status quality.ScoreStatus, score float64) agentbench.TaskScore {
	return agentbench.TaskScore{
		TaskID: task, Repeat: 1, Kind: quality.ScoreKindPinnedCaseValidation,
		Status: status, Score: score,
	}
}

func TestPrintScoreCoverage_NamesBothExclusionsBesideTheMean(t *testing.T) {
	var buf bytes.Buffer
	printScoreCoverage(&buf, []agentbench.TaskScore{
		row("dp-01", quality.ScoreStatusScored, 1.0),
		row("dp-02", quality.ScoreStatusScored, 0.5),
		row("dp-03", quality.ScoreStatusUnscorable, 0.0),
		row("sw-01", quality.ScoreStatusNotApplicable, 0.0),
	})
	out := buf.String()

	// The mean is over the two SCORED rows, not over four with two zeros.
	if !strings.Contains(out, "0.7500") {
		t.Fatalf("mean is not over the scored set alone: %s", out)
	}
	for _, want := range []string{"2 of 3 contract-declaring", "1 unscorable", "1 not_applicable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in: %s", want, out)
		}
	}
}

// Everything unscorable must not print as 0.0000 — that reads as "the agent
// scored zero" when it means "nothing here could be scored".
func TestPrintScoreCoverage_NoScoredRowsDoesNotPrintZero(t *testing.T) {
	var buf bytes.Buffer
	printScoreCoverage(&buf, []agentbench.TaskScore{
		row("dp-01", quality.ScoreStatusUnscorable, 0.0),
	})
	out := buf.String()
	if strings.Contains(out, "0.0000") {
		t.Fatalf("an all-unscorable arm printed a zero mean: %s", out)
	}
	if !strings.Contains(out, "no scored task") {
		t.Fatalf("want an explicit no-scored-task line, got: %s", out)
	}
}

// A journal with no graded rows at all prints nothing: absence of a contract is
// not a quality statement.
func TestPrintScoreCoverage_SilentWhenNoContractDeclared(t *testing.T) {
	var buf bytes.Buffer
	printScoreCoverage(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("want silence, got %q", buf.String())
	}
}

// An arm that never exercised the rework loop must say so where the number is
// read, not in a design nobody re-opens.
func TestPrintScoreCoverage_NamesAStraightLineArm(t *testing.T) {
	var buf bytes.Buffer
	straight := row("dp-01", quality.ScoreStatusScored, 1.0)
	straight.ProducerVisits, straight.VerifierVisits = 1, 1
	printScoreCoverage(&buf, []agentbench.TaskScore{straight})

	if !strings.Contains(buf.String(), "NOT EXERCISED") {
		t.Fatalf("a straight-line arm printed no warning: %s", buf.String())
	}
}
