package membench

import (
	"context"
	"math"
	"strings"
	"testing"
)

// Tier-2-only mode: score retrieval, skip answering and judging.
//
// This is the unlock for a retrieval CI gate. Judged accuracy has sd ~4.5% at
// n=30 and cannot be gated — it would fire on judge noise — whereas the RRF
// retrieval path measured sd 0.0000. So the gate scores tier-2 only, which needs
// no judge, no answer generation, no reranker, and therefore no cloud credentials
// and nothing billable. Without this mode even a ten-item screen spends cloud
// calls it does not need, which is what makes the gate unaffordable per-change.
//
// The hazard is not the skipping. It is that a run which never judged anything
// must not read as a run that judged everything wrong, and must not compare clean
// against a judged run. Both have precedent here: the 2026-08-11 pre-fix runs
// shared a byte-identical comparability key with post-fix reranked runs because
// the key carried no recall method, and two different experiments compared as one.

func TestOutcomeCounts_UnjudgedIsNotIncorrect(t *testing.T) {
	c := OutcomeCounts{Unjudged: 10}

	if got := c.Judged(); got != 0 {
		t.Errorf("Judged() = %d, want 0 — nothing was judged", got)
	}
	if acc := c.Accuracy(); !math.IsNaN(acc) {
		t.Errorf("Accuracy() = %v, want NaN — 'never judged' must not read as 0%% correct, "+
			"which is what a reader would see if this returned zero", acc)
	}
	// The items are still part of the run's population.
	if got := c.Total(); got != 10 {
		t.Errorf("Total() = %d, want 10 — unjudged items still ran", got)
	}
	// And nothing FAILED, so the run is not degraded. An unjudged item told us
	// about retrieval, which is the opposite of telling us nothing.
	if got := c.DegradedRate(); got != 0 {
		t.Errorf("DegradedRate() = %v, want 0", got)
	}
}

func TestOutcomeCounts_UnjudgedDoesNotDiluteAccuracy(t *testing.T) {
	// A mixed population should not arise from one run, but the arithmetic must
	// still be right: accuracy is over JUDGED, so unjudged items cannot move it.
	c := OutcomeCounts{Correct: 3, Incorrect: 1, Unjudged: 96}
	if got := c.Accuracy(); got != 0.75 {
		t.Errorf("Accuracy() = %v, want 0.75 (3 of 4 judged) — unjudged items must not "+
			"enter the denominator", got)
	}
}

func TestOutcomeCounts_AddTracksUnjudged(t *testing.T) {
	var c OutcomeCounts
	c.Add(OutcomeUnjudged)
	if c.Unjudged != 1 {
		t.Errorf("Add(OutcomeUnjudged) did not tally: %+v", c)
	}
	if c.Correct+c.Incorrect+c.Invalid+c.Error != 0 {
		t.Errorf("Add(OutcomeUnjudged) leaked into another bucket: %+v", c)
	}
}

// TestTier2Only_RunsWithNoGeneratorAndNoJudge is both the affordability assertion
// and the strongest available proof that no tier-1 call is made: a nil Generator
// and nil Judge would panic the moment either was used. A call counter could only
// show a call was not counted; nil shows it cannot have happened.
//
// It also states the mode's reason for existing — a gate that needed a judge could
// not run on a fork PR, which is what rules Tier-1 out of CI entirely.
func TestTier2Only_RunsWithNoGeneratorAndNoJudge(t *testing.T) {
	sys := newFakeSystem("fake")
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		Generator: nil,
		Judge:     nil,
		RunDir:    t.TempDir(),
		MaxTokens: 4096,
		Tier2Only: true,
	}

	res, err := r.Run(context.Background(), "")
	if err != nil {
		t.Fatalf("tier-2-only run with no generator and no judge must be allowed: %v", err)
	}

	// Tier-2 metrics must still be produced — that is the output a gate reads.
	total := 0
	for cat, m := range res.Metrics {
		total += m.Scored
		if m.Scored > 0 && m.ContextRecall == 0 {
			t.Errorf("category %q scored %d items with zero recall; the fake system returns "+
				"the whole scope, so the gold document was certainly retrieved", cat, m.Scored)
		}
	}
	if total != len(simpleItems()) {
		t.Errorf("scored %d items, want %d — retrieval must be scored even though nothing "+
			"was judged", total, len(simpleItems()))
	}

	// The counts must say "unjudged", not "incorrect".
	for cat, c := range res.Counts {
		if c.Unjudged == 0 {
			t.Errorf("category %q recorded no unjudged items: %+v", cat, c)
		}
		if c.Correct != 0 || c.Incorrect != 0 {
			t.Errorf("category %q claims judged outcomes in a tier-2-only run: %+v", cat, c)
		}
	}
}

// TestJudgedRun_RefusesWithoutJudge is the other direction, and the reason the mode
// is an explicit flag rather than "judge if a judge happens to be wired". An
// implicit fallback would let a misconfigured full run silently report itself as a
// retrieval-only run — absence reading as a decision, which is the failure shape
// this harness keeps finding elsewhere.
func TestJudgedRun_RefusesWithoutJudge(t *testing.T) {
	sys := newFakeSystem("fake")
	r := &Runner{
		System:    sys,
		Dataset:   oneItemDataset{name: "test", items: simpleItems()},
		RunDir:    t.TempDir(),
		MaxTokens: 4096,
		Tier2Only: false,
	}

	_, err := r.Run(context.Background(), "")
	if err == nil {
		t.Fatal("a judged run with no judge must be refused, not silently downgraded to tier-2")
	}
	if !strings.Contains(err.Error(), "Tier2Only") && !strings.Contains(err.Error(), "judge") {
		t.Errorf("error %q should name the missing judge or the flag that would make its "+
			"absence legitimate", err)
	}
}

// TestTier2Only_IsInTheComparabilityKey applies the lesson of 2026-08-11 before it
// can bite again: a run that skipped judging is a DIFFERENT experiment from one
// that judged, and two different experiments must not share a key.
func TestTier2Only_IsInTheComparabilityKey(t *testing.T) {
	judged := ComparabilityFields{
		HarnessVersion: "v1", DatasetSHA256: "abc",
		AnswerModel: "m", JudgeModel: "j",
	}
	tier2 := judged
	tier2.Tier2Only = true

	if judged.Key() == tier2.Key() {
		t.Error("a tier-2-only run and a judged run produced the SAME comparability key — " +
			"that is precisely how the pre-fix and post-fix reranker runs compared clean " +
			"on 2026-08-11")
	}
}
