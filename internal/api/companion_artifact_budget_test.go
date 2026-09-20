package api

import (
	"strings"
	"testing"
)

// The observed run: 248 KB of staged artifacts against a 100k context, which
// the container's own preflight measured at 93,346 prompt tokens on iteration 1
// and proceeded anyway — four iterations, ~$0.27, and a shape-contract failure
// instead of a review.
func TestStagedArtifactBudget_RefusesTheObservedRun(t *testing.T) {
	err := checkStagedArtifactBudget("companion-architectural-review", 248*1024, 100000, 16384)
	if err == nil {
		t.Fatal("248 KB against a 100k context was admitted")
	}
	for _, want := range []string{"ARTIFACTS_EXCEED_CONTEXT", "248 KB", "Split the upload"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

// The workaround that actually worked on the day — three diff-only reviews of
// ~39 KB, ~51 KB and ~76 KB — must all pass, or the guard would have refused
// the remedy it recommends.
func TestStagedArtifactBudget_AdmitsTheSplitThatWorked(t *testing.T) {
	for _, kb := range []int{39, 51, 76} {
		if err := checkStagedArtifactBudget("companion-architectural-review", kb*1024, 100000, 16384); err != nil {
			t.Fatalf("%d KB was refused, but this split is what the guard tells callers to do: %v", kb, err)
		}
	}
}

// Unmeasurable is not refusable. A daemon that does not declare a context size
// must not have every artifact delegation fail.
func TestStagedArtifactBudget_UnknownContextAdmits(t *testing.T) {
	if err := checkStagedArtifactBudget("wf", 10*1024*1024, 0, 0); err != nil {
		t.Fatalf("an unknown context size became a refusal: %v", err)
	}
	if err := checkStagedArtifactBudget("wf", 0, 100000, 16384); err != nil {
		t.Fatalf("a delegation with no artifacts was refused: %v", err)
	}
}

// A large context is exactly what a big upload is for. The guard must scale
// with the window rather than enforcing a fixed byte ceiling.
func TestStagedArtifactBudget_ScalesWithTheWindow(t *testing.T) {
	if err := checkStagedArtifactBudget("wf", 248*1024, 1000000, 32768); err != nil {
		t.Fatalf("248 KB against a 1M context was refused: %v", err)
	}
}

// The numbers must be IN the message: a caller that is told only "too large"
// cannot tell a 10% overshoot from a 10x one, and the split differs.
func TestStagedArtifactBudget_RefusalCarriesItsArithmetic(t *testing.T) {
	err := checkStagedArtifactBudget("wf", 400*1024, 100000, 16384)
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	// ~400 KB / 3 ≈ 136k tokens against a ceiling of (100000-16384)/2 ≈ 41808.
	if !strings.Contains(msg, "136") || !strings.Contains(msg, "41808") {
		t.Fatalf("the refusal does not carry its arithmetic: %s", msg)
	}
}
