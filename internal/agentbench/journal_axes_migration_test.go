package agentbench

import (
	"strings"
	"testing"
)

// resolvableFloor is the delta these fixtures can resolve. One value, named
// once: the migration behaviour under test does not depend on it, and threading
// it through every call site would suggest it does.
const resolvableFloor = 0.04

func journalWithAxes(arm ArmFields, axes []string) Journal {
	return Journal{Manifest: RunManifest{
		Arm:             arm,
		PreRegistration: PreRegistration{IndependentAxes: axes},
		Power:           PowerCheck{ResolvableDelta: resolvableFloor},
	}}
}

// Regression, 2026-08-16. IndependentAxes was added on 2026-08-15 and
// CompareJournals required both sides to declare the SAME axes. Every journal
// written before that has the field absent, so the check made every
// pre-existing baseline permanently incomparable with every new run — which
// forbids the normal workflow of registering a new arm against an existing
// baseline. It surfaced when the qwen-local baseline (n=10, complete) could not
// be compared against the arm run specifically to price fixes against it.
//
// An empty declaration is "did not say", not "said something different".
func TestCompareJournals_AllowsAnUndeclaredLegacyBaseline(t *testing.T) {
	oldArm := ArmFields{HarnessVersion: "3", BinarySHA256: "aaa", ConfigSHA256: "cfg", TaskSetSHA256: "ts", GoldSHA256: "g"}
	newArm := oldArm
	newArm.BinarySHA256 = "bbb" // the axis under test

	legacy := journalWithAxes(oldArm, nil) // predates the field
	current := journalWithAxes(newArm, []string{"binary_sha256"})

	got, err := CompareJournals(legacy, current, 0.09)
	if err != nil {
		t.Fatalf("a legacy baseline must remain comparable: %v", err)
	}
	// The result must SAY it was not verified — allowing the pair silently
	// would trade one defect for a worse one.
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("verdict %q must mark the comparison unverified", got)
	}
	if !strings.Contains(got, "binary_sha256") {
		t.Errorf("verdict %q must name the axis that was assumed", got)
	}
}

// Two runs that BOTH declared, and disagree, are still refused: that is a real
// mismatch of experiments rather than a missing declaration.
func TestCompareJournals_StillRefusesGenuinelyDifferentAxes(t *testing.T) {
	arm := ArmFields{HarnessVersion: "3", BinarySHA256: "aaa"}
	a := journalWithAxes(arm, []string{"binary_sha256"})
	b := journalWithAxes(arm, []string{"config_sha256"})

	if _, err := CompareJournals(a, b, 0.09); err == nil {
		t.Fatal("two differing declarations must still be refused")
	}
}

// With neither side declaring, every axis must still match exactly — the
// migration allowance must not become a general loosening.
func TestCompareJournals_NeitherDeclared_StillDemandsAnIdenticalKey(t *testing.T) {
	a := journalWithAxes(ArmFields{HarnessVersion: "3", BinarySHA256: "aaa"}, nil)
	b := journalWithAxes(ArmFields{HarnessVersion: "3", BinarySHA256: "bbb"}, nil)

	if _, err := CompareJournals(a, b, 0.09); err == nil {
		t.Fatal("with no declared axis, a differing binary must still be refused")
	}
}
