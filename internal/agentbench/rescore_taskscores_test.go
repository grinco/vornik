package agentbench

import (
	"context"
	"strings"
	"testing"
)

type nopTraces struct{}

func (nopTraces) AssembleTraces(context.Context, string) ([]Trace, error) { return nil, nil }

// Rescore recomputes Records[].Verdicts and does NOT touch TaskScores — they
// pass through verbatim. That was harmless while every contract change was a
// PROBE change. The 2026-09-17 extras fix is a TASK-SCORE change, so a rescored
// journal would carry v5 pinned-case scores under a v6 stamp: it would assert it
// was scored under the current contract, and for the metric that gates the
// release it would not have been.
//
// Verified against the real arm: dp-01-nilguard rescored from harness 5 to 6 and
// kept 0.000/invalid_evidence, while the current scorer on the same stored
// snapshot yields 1.000 with extraCaseCount 3.
//
// Until Rescore can recompute task scores it must REFUSE a journal that carries
// them, rather than mislabel one. Silence here is the failure mode this
// codebase keeps recording.
func TestRescore_RefusesJournalCarryingTaskScores(t *testing.T) {
	j := Journal{}
	j.Manifest.RunID = "r1"
	j.Manifest.Arm.HarnessVersion = "5"
	j.TaskScores = []TaskScore{{TaskID: "dp-01", Repeat: 1, Score: 0}}

	_, err := Rescore(context.Background(), j, nopTraces{}, []Probe{SchemaProbe{}}, nil)
	if err == nil {
		t.Fatal("rescored a journal carrying task scores: the result would be stamped " +
			"with the current harness while its release metric was computed under the old one")
	}
	if !strings.Contains(err.Error(), "task score") {
		t.Errorf("refusal should name task scores, got: %v", err)
	}
}

// A journal with no task scores is pure probe evidence and still rescores.
func TestRescore_StillWorksWithoutTaskScores(t *testing.T) {
	j := Journal{}
	j.Manifest.RunID = "r2"
	j.Manifest.Arm.HarnessVersion = "5"

	out, err := Rescore(context.Background(), j, nopTraces{}, []Probe{SchemaProbe{}}, nil)
	if err != nil {
		t.Fatalf("probe-only journal must still rescore: %v", err)
	}
	if out.Manifest.Arm.HarnessVersion != HarnessVersion {
		t.Errorf("harness version = %q, want %q", out.Manifest.Arm.HarnessVersion, HarnessVersion)
	}
}
