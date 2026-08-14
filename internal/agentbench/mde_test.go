package agentbench

import (
	"math"
	"strings"
	"testing"
)

func TestRequiredPairs_MatchesTheStatedFormula(t *testing.T) {
	// n >= 7.849 * (sigma/delta)^2. At sigma == delta that is 8 pairs.
	n, err := RequiredPairs(0.05, 0.05)
	if err != nil {
		t.Fatalf("required pairs: %v", err)
	}
	if n != 8 {
		t.Errorf("n = %d, want 8", n)
	}

	// Halving the effect quadruples the requirement: (sigma/delta)^2.
	n, err = RequiredPairs(0.05, 0.025)
	if err != nil {
		t.Fatalf("required pairs: %v", err)
	}
	if n != 32 {
		t.Errorf("n = %d, want 32 — halving delta must quadruple n", n)
	}
}

func TestRequiredPairs_RefusesInputsThatCannotSizeARun(t *testing.T) {
	if _, err := RequiredPairs(0, 0.05); err == nil {
		t.Error("sized a run from an unmeasured sigma — §5.4 forbids gating before the " +
			"noise-floor pass")
	}
	if _, err := RequiredPairs(0.05, 0); err == nil {
		t.Error("sized a run for a zero effect, which needs infinitely many pairs")
	}
}

func TestResolvableDelta_InvertsRequiredPairs(t *testing.T) {
	const sigma = 0.05
	n, err := RequiredPairs(sigma, 0.025)
	if err != nil {
		t.Fatalf("required pairs: %v", err)
	}
	got, err := ResolvableDelta(sigma, n)
	if err != nil {
		t.Fatalf("resolvable delta: %v", err)
	}
	// n was rounded up, so the resolvable delta is at or slightly below target.
	if got > 0.025+1e-9 {
		t.Errorf("resolvable delta = %v, want <= 0.025", got)
	}
	if math.Abs(got-0.025) > 0.002 {
		t.Errorf("resolvable delta = %v, expected close to 0.025", got)
	}
}

// A fixed task set cannot grow, so the honest output is the effect it CAN see.
func TestPowerCheck_UnderpoweredRunNamesWhatItCanResolve(t *testing.T) {
	p, err := CheckPower(0.05, 12, 0.01, 20)
	if err != nil {
		t.Fatalf("check power: %v", err)
	}
	if p.Adequate {
		t.Fatal("20 pairs reported adequate for a delta needing far more")
	}

	err = p.Refuse()
	if err == nil {
		t.Fatal("an underpowered run was allowed to gate")
	}
	msg := err.Error()
	if !strings.Contains(msg, "INCONCLUSIVE") {
		t.Errorf("refusal does not steer toward inconclusive: %v", err)
	}
	if !strings.Contains(msg, "can resolve") {
		t.Errorf("refusal does not name the resolvable delta, leaving an operator to "+
			"work out what their pairs are good for — and the usual answer is to run "+
			"it anyway: %v", err)
	}
}

func TestPowerCheck_AdequateRunPasses(t *testing.T) {
	p, err := CheckPower(0.01, 12, 0.02, 20)
	if err != nil {
		t.Fatalf("check power: %v", err)
	}
	if !p.Adequate {
		t.Fatalf("20 pairs reported inadequate for a large effect: needs %d", p.RequiredPairs)
	}
	if err := p.Refuse(); err != nil {
		t.Errorf("an adequately powered run was refused: %v", err)
	}
}

// An sigma from n=4 is not an sigma. The 2026-08-11 measurement moved context
// precision 0.0082 -> 0.0195 between n=4 and n=10, turning an apparent 3.4-sigma
// effect into 1.6-sigma. Underestimating spread manufactures significance.
func TestPowerCheck_RefusesASigmaMeasuredFromTooFewRuns(t *testing.T) {
	p, err := CheckPower(0.01, 4, 0.05, 40)
	if err != nil {
		t.Fatalf("check power: %v", err)
	}
	if !p.Adequate {
		t.Fatal("precondition: this run is amply powered on paper")
	}

	err = p.Refuse()
	if err == nil {
		t.Fatal("gated on a sigma measured from 4 runs — being amply powered against a " +
			"wrong sigma is worse than being underpowered against a right one")
	}
	if !strings.Contains(err.Error(), "n=4") {
		t.Errorf("refusal does not name the offending n: %v", err)
	}
}

func TestPowerCheck_CarriesSigmaNIntoTheReport(t *testing.T) {
	p, err := CheckPower(0.02, 15, 0.05, 30)
	if err != nil {
		t.Fatalf("check power: %v", err)
	}
	if p.SigmaN != 15 {
		t.Errorf("sigmaN = %d, want 15 — the n a sigma came from must travel with it "+
			"into every report", p.SigmaN)
	}
}
