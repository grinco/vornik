package cli

import (
	"strings"
	"testing"
)

// Finding D of the 2026-08-26 silent-controls audit: a control with a coverage
// boundary publishes a denominator. `workflow-stats` already lists top failure
// classes as bare counts, and "104 container_non_zero_exit" was read as a fleet
// total when it was one workflow over 30 days — the number that made the whole
// residual bucket look ten times smaller than it was.
//
// The denominator CANNOT be the sum of TopFailureClasses: that list is capped
// at 10 by fillTopFailureClasses, so summing it silently under-reports on any
// workflow with more than ten distinct classes — the same defect one layer up.
//
// Design: https://docs.vornik.io (D7)
func TestRenderUnclassifiedShare_PublishesTheDenominator(t *testing.T) {
	got := renderUnclassifiedShare(&workflowStatsResponse{
		UnclassifiedStepFailures: 92,
		ClassifiedStepFailures:   934,
	})
	for _, want := range []string{"92", "934", "9.9%"} {
		if !strings.Contains(got, want) {
			t.Errorf("share line %q must carry %q", got, want)
		}
	}
}

// Zero unclassified against real failures is a reportable result and must
// still print — it is the evidence that the classifier is working.
func TestRenderUnclassifiedShare_ZeroOfManyStillPrints(t *testing.T) {
	got := renderUnclassifiedShare(&workflowStatsResponse{
		UnclassifiedStepFailures: 0,
		ClassifiedStepFailures:   271,
	})
	if got == "" {
		t.Fatal("zero unclassified of 271 is evidence and must be shown")
	}
	if !strings.Contains(got, "271") {
		t.Errorf("must publish the denominator: %q", got)
	}
}

// No classified failures at all is no evidence, not a healthy classifier.
// Printing "0.0% unclassified" there would assert coverage that does not
// exist — the same conflation the audit is about.
func TestRenderUnclassifiedShare_NoFailuresPrintsNothing(t *testing.T) {
	if got := renderUnclassifiedShare(&workflowStatsResponse{}); got != "" {
		t.Errorf("an empty window must not render a share; got %q", got)
	}
}
