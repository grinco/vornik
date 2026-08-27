package api

import (
	"strings"
	"testing"
)

// Finding D of docs/audits/2026-08-26-silent-controls-audit.md: a control with
// a coverage boundary must publish a DENOMINATOR. "Zero findings" and "zero
// coverage" rendered identically, and the unclassified bucket is exactly that
// shape — a bare count of 302 says nothing without the 5,796 it is drawn from.
//
// Design: https://docs.vornik.io (D7)

// The Finding A contract, which this check must not violate on its way in: a
// check that could not evaluate reports SKIPPED, never OK. An empty window is
// not a healthy classifier — it is no evidence at all.
func TestUnclassifiedShare_EmptyWindowIsSkippedNotOK(t *testing.T) {
	got := evaluateUnclassifiedShare(0, 0, 0.15)
	if got.Status != "SKIPPED" {
		t.Fatalf("a window with no failed steps must be SKIPPED, got %q (%s)", got.Status, got.Message)
	}
}

func TestUnclassifiedShare_AboveThresholdWarns(t *testing.T) {
	got := evaluateUnclassifiedShare(40, 100, 0.15)
	if got.Status != "WARNING" {
		t.Fatalf("40%% unclassified against a 15%% threshold must WARN, got %q", got.Status)
	}
	// A classifier whose modal output is "unknown" is the finding, not the
	// baseline — the message has to say how bad, against what.
	for _, want := range []string{"40", "100", "15"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message must carry the share, the denominator and the threshold; %q lacks %q", got.Message, want)
		}
	}
}

func TestUnclassifiedShare_BelowThresholdIsOK(t *testing.T) {
	got := evaluateUnclassifiedShare(9, 100, 0.15)
	if got.Status != "OK" {
		t.Fatalf("9%% against a 15%% threshold must be OK, got %q", got.Status)
	}
}

// Publishing the denominator is the point of the check, so it appears even
// when the check passes. A green check that hides its coverage is the defect
// this whole class is about.
func TestUnclassifiedShare_OKStillPublishesTheDenominator(t *testing.T) {
	got := evaluateUnclassifiedShare(9, 100, 0.15)
	if !strings.Contains(got.Message, "100") {
		t.Errorf("a passing check must still publish its denominator: %q", got.Message)
	}
}

// Exactly at the threshold is not over it. Chosen deliberately so a threshold
// set to the measured steady state does not warn permanently.
func TestUnclassifiedShare_AtThresholdDoesNotWarn(t *testing.T) {
	got := evaluateUnclassifiedShare(15, 100, 0.15)
	if got.Status != "OK" {
		t.Fatalf("exactly at the threshold must not warn, got %q (%s)", got.Status, got.Message)
	}
}

// Zero unclassified in a window that HAD failures is a real, reportable
// result — distinct from the empty window above.
func TestUnclassifiedShare_ZeroOfManyIsOKNotSkipped(t *testing.T) {
	got := evaluateUnclassifiedShare(0, 271, 0.15)
	if got.Status != "OK" {
		t.Fatalf("zero unclassified of 271 failures is OK, got %q", got.Status)
	}
	if got.Status == "SKIPPED" {
		t.Fatal("a window with failures is evidence, not an absence of it")
	}
}
