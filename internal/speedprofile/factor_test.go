package speedprofile

import (
	"math"
	"strings"
	"testing"
)

func cfg() FactorConfig {
	return FactorConfig{ReferenceTokensPerSec: 200, MinFactor: 0.5, MaxFactor: 8}
}

func TestFactor_SlowerHardwareWantsMoreTime(t *testing.T) {
	// Half the reference speed wants twice the time.
	f, clamped, err := Factor(100, cfg())
	if err != nil || clamped {
		t.Fatalf("unexpected: f=%v clamped=%v err=%v", f, clamped, err)
	}
	if math.Abs(f-2) > 0.001 {
		t.Errorf("factor = %.3f, want 2", f)
	}
}

// The measured slow host: 12 tok/s against a 200 tok/s reference wants ~17x,
// past any sane ceiling. It must clamp AND say so — an operator who is not told
// sees an ordinary timeout with nothing to suggest the budget was never adequate.
func TestFactor_ReportsWhenHardwareWantsMoreThanAllowed(t *testing.T) {
	f, clamped, err := Factor(12, cfg())
	if err != nil {
		t.Fatalf("factor: %v", err)
	}
	if !clamped {
		t.Error("a 17x demand was silently satisfied; the clamp is invisible")
	}
	if f != 8 {
		t.Errorf("factor = %.1f, want the ceiling of 8", f)
	}
}

// Faster hardware must not starve a step below the floor.
func TestFactor_FastHardwareIsFlooredNotShrunkAway(t *testing.T) {
	f, _, err := Factor(100000, cfg())
	if err != nil {
		t.Fatalf("factor: %v", err)
	}
	if f != 0.5 {
		t.Errorf("factor = %.3f, want the floor of 0.5", f)
	}
}

// No profile yet is the normal state of a fresh deployment. Scaling must never
// be a startup dependency.
func TestFactor_UnknownProfileChangesNothing(t *testing.T) {
	f, clamped, err := Factor(0, cfg())
	if err != nil || clamped || f != 1 {
		t.Errorf("unprofiled host got f=%v clamped=%v err=%v; want an untouched 1", f, clamped, err)
	}
}

// Inferring the reference locally would make every deployment its own baseline
// and the factor would collapse to 1.0 everywhere.
func TestFactor_RefusesAnUndeclaredReference(t *testing.T) {
	_, _, err := Factor(100, FactorConfig{MinFactor: 0.5, MaxFactor: 8})
	if err == nil {
		t.Fatal("a missing reference was accepted")
	}
	if !strings.Contains(err.Error(), "slower THAN") {
		t.Errorf("error does not explain why a reference is needed: %v", err)
	}
}
