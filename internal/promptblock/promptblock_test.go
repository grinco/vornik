package promptblock

import (
	"reflect"
	"testing"
)

// TestReportingIntegrityIsInvariant pins the one classification that carries a
// rule rather than a hint. verifyRoleClaims runs on every step of every
// deployment whatever the prompt says, so reclassifying this block advisory
// would let an operator suppress being TOLD about a check they are still
// subject to — the failure the class exists to prevent.
func TestReportingIntegrityIsInvariant(t *testing.T) {
	c, ok := ClassOf(ReportingIntegrity)
	if !ok {
		t.Fatal("reporting-integrity is not declared at all")
	}
	if c != Invariant {
		t.Errorf("reporting-integrity is classed %q, want %q", c, Invariant)
	}
	if Suppressible(ReportingIntegrity) {
		t.Error("reporting-integrity reports as suppressible; suppressing it would " +
			"remove the warning and not the rule")
	}
}

func TestClassOfAndKnown(t *testing.T) {
	cases := []struct {
		name      string
		wantClass Class
		wantKnown bool
	}{
		{CanonicalContext, Advisory, true},
		{ToolBudget, Advisory, true},
		{ReportingIntegrity, Invariant, true},
		{"tool-budgett", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := ClassOf(tc.name)
			if ok != tc.wantKnown {
				t.Fatalf("Known = %v, want %v", ok, tc.wantKnown)
			}
			if c != tc.wantClass {
				t.Errorf("ClassOf = %q, want %q", c, tc.wantClass)
			}
			if Known(tc.name) != tc.wantKnown {
				t.Errorf("Known(%q) disagrees with ClassOf", tc.name)
			}
		})
	}
}

func TestSuppressible(t *testing.T) {
	if !Suppressible(CanonicalContext) || !Suppressible(ToolBudget) {
		t.Error("advisory blocks must be suppressible")
	}
	if Suppressible("nope") {
		t.Error("an unknown name must not report as suppressible")
	}
}

// TestNamesAreSortedAndComplete keeps the error-message helpers honest: an
// operator who mistypes a block name is shown the full set, so a name missing
// from Names() is a name they cannot discover from the failure.
func TestNamesAreSortedAndComplete(t *testing.T) {
	want := []string{CanonicalContext, ReportingIntegrity, ToolBudget, WorkspaceGit}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v (sorted, complete)", got, want)
	}
	wantSuppressible := []string{CanonicalContext, ToolBudget}
	if got := SuppressibleNames(); !reflect.DeepEqual(got, wantSuppressible) {
		t.Errorf("SuppressibleNames() = %v, want %v", got, wantSuppressible)
	}
	if len(Names()) != len(classByName) {
		t.Errorf("Names() returned %d of %d declared blocks", len(Names()), len(classByName))
	}
}

// TestClassIsFixedAtDefinition guards LLD 09 §13.7 invariant 4: a class is a
// property of the block, not of a selection. Nothing exported may set one — if a
// setter ever appears, a selector could "pin" an advisory block by promoting it
// to invariant, which invariant 2 forbids.
func TestClassIsFixedAtDefinition(t *testing.T) {
	for _, name := range Names() {
		before, _ := ClassOf(name)
		// Exercise every read path; none may mutate.
		_ = Suppressible(name)
		_ = Known(name)
		after, _ := ClassOf(name)
		if before != after {
			t.Errorf("block %q changed class from %q to %q by being read", name, before, after)
		}
	}
}
