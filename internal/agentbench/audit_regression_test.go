package agentbench

import (
	"math"
	"strings"
	"testing"
)

func readableJournal() Journal {
	arm := ArmFields{
		HarnessVersion: HarnessVersion, BinarySHA256: "bin", ConfigSHA256: "cfg",
		Models: map[string]string{"lead": "model"}, ContextPolicy: "default",
		TaskSetSHA256: "tasks", Probes: []string{"schema"},
	}
	return Journal{Manifest: RunManifest{
		Arm: arm, ArmKey: arm.Key(), PreRegistrationHash: "pre",
		Power: PowerCheck{SigmaD: 1, SigmaN: MinimumSigmaRuns, TargetDelta: 1,
			RequiredPairs: 8, AvailablePairs: 10, ResolvableDelta: .886, Adequate: true},
	}}
}

func TestCompareJournalsRefusesUnreadableInputs(t *testing.T) {
	a, b := readableJournal(), readableJournal()
	a.Manifest.Untrustworthy = true
	a.Manifest.UntrustworthyReason = "mixed harnesses"
	if _, err := CompareJournals(a, b, 1); err == nil || !strings.Contains(err.Error(), "first journal") {
		t.Fatalf("err = %v, want unreadable first journal refusal", err)
	}
}

func TestCompareJournalsUsesAbsoluteDeltaAgainstFloor(t *testing.T) {
	a, b := readableJournal(), readableJournal()
	got, err := CompareJournals(a, b, -1.0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "INCONCLUSIVE") {
		t.Fatalf("%q: magnitude 1.0 clears the configured floor", got)
	}
}

func TestPowerMathRejectsNonFiniteInputs(t *testing.T) {
	for _, tc := range []struct{ sigma, delta float64 }{
		{math.NaN(), 1}, {1, math.NaN()}, {math.Inf(1), 1}, {1, math.Inf(1)},
	} {
		if _, err := RequiredPairs(tc.sigma, tc.delta); err == nil {
			t.Errorf("RequiredPairs(%v,%v) accepted non-finite input", tc.sigma, tc.delta)
		}
	}
	if _, err := ResolvableDelta(math.NaN(), 10); err == nil {
		t.Fatal("ResolvableDelta accepted NaN")
	}
}

func TestPowerCheckRefuseValidatesSerializedInvariants(t *testing.T) {
	bad := []PowerCheck{
		{Adequate: true},
		{SigmaD: 1, SigmaN: 0, TargetDelta: 1, RequiredPairs: 1, AvailablePairs: 1, ResolvableDelta: 1, Adequate: true},
		{SigmaD: 1, SigmaN: MinimumSigmaRuns, TargetDelta: 1, RequiredPairs: 99, AvailablePairs: 1, ResolvableDelta: 1, Adequate: true},
	}
	for i, p := range bad {
		if err := p.Refuse(); err == nil {
			t.Errorf("case %d accepted malformed serialized power data: %+v", i, p)
		}
	}
}

func TestPreRegistrationValidatesIndependentAxesAndPowerFields(t *testing.T) {
	base := PreRegistration{Arms: []string{"a", "b"}, Metric: "m", TargetDelta: 1,
		SigmaD: 1, SigmaN: MinimumSigmaRuns, ComputedPairs: 8, Rationale: "because"}
	for _, mutate := range []func(*PreRegistration){
		func(p *PreRegistration) { p.IndependentAxes = []string{"unknown"} },
		func(p *PreRegistration) { p.IndependentAxes = []string{"binary_sha256", "binary_sha256"} },
		func(p *PreRegistration) { p.SigmaD = 0 },
		func(p *PreRegistration) { p.SigmaN = 0 },
		func(p *PreRegistration) { p.ComputedPairs = 0 },
	} {
		p := base
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("invalid pre-registration accepted: %+v", p)
		}
	}
}

func TestArmPartialRequiresIdentityBearingFields(t *testing.T) {
	base := ArmFields{HarnessVersion: HarnessVersion, BinarySHA256: "b", ConfigSHA256: "c",
		Models: map[string]string{"lead": "m"}, AgentImages: map[string]string{"lead": "sha256:a"},
		ContextPolicy: "p", TaskSetSHA256: "t", TierPolicySHA256: "tiers", Probes: []string{"schema"}}
	if base.Partial() {
		t.Fatal("complete arm marked partial")
	}
	for _, broken := range []ArmFields{
		func() ArmFields { x := base; x.HarnessVersion = ""; return x }(),
		func() ArmFields { x := base; x.Models = nil; return x }(),
		func() ArmFields { x := base; x.AgentImages = nil; return x }(),
		func() ArmFields { x := base; x.TierPolicySHA256 = ""; return x }(),
		func() ArmFields { x := base; x.ContextPolicy = ""; return x }(),
		func() ArmFields { x := base; x.Probes = nil; return x }(),
	} {
		if !broken.Partial() {
			t.Errorf("incomplete arm marked complete: %+v", broken)
		}
	}
}
