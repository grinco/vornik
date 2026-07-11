package projectwizard

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/rolelibrary"
)

func TestEstimateCostBand_NilOrIncomplete(t *testing.T) {
	if got := estimateCostBand(nil); got != "" {
		t.Errorf("nil bundle: got %q, want empty", got)
	}
	if got := estimateCostBand(&materializedBundle{}); got != "" {
		t.Errorf("bundle with nil Project: got %q, want empty", got)
	}
}

func TestEstimateCostBand_OneShot_NoAutonomy(t *testing.T) {
	mb := &materializedBundle{
		Project:        &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: false}},
		RoleModelTiers: map[string]string{"researcher": rolelibrary.ModelTierStandard, "writer": rolelibrary.ModelTierStandard},
	}
	got := estimateCostBand(mb)
	if !strings.Contains(got, "estimate") {
		t.Errorf("expected the estimate label, got %q", got)
	}
	if !strings.Contains(got, "one-time run") {
		t.Errorf("expected one-shot framing for a non-autonomous bundle, got %q", got)
	}
	// 2 standard-tier roles: perRun = 2 * standardTierCostPerRunUSD.
	wantPerRun := 2 * standardTierCostPerRunUSD
	wantPrefix := formatCostBand(wantPerRun, wantPerRun, true)
	if got != wantPrefix {
		t.Errorf("got %q, want %q", got, wantPrefix)
	}
}

func TestEstimateCostBand_DailySchedule_ProducesMonthlyBand(t *testing.T) {
	mb := &materializedBundle{
		Project: &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "24h"}},
		RoleModelTiers: map[string]string{
			"researcher": rolelibrary.ModelTierStandard,
			"writer":     rolelibrary.ModelTierStandard,
		},
	}
	got := estimateCostBand(mb)
	if !strings.Contains(got, "estimate") {
		t.Errorf("expected the estimate label, got %q", got)
	}
	if !strings.Contains(got, "per month") {
		t.Errorf("expected a monthly band for a recurring schedule, got %q", got)
	}
	wantPerRun := 2 * standardTierCostPerRunUSD
	wantPerMonth := wantPerRun * 30 // 30*24h / 24h = 30 runs/month
	want := formatCostBand(wantPerRun, wantPerMonth, false)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEstimateCostBand_OneShotVsDaily_DifferentMonthlyBands(t *testing.T) {
	tiers := map[string]string{"researcher": rolelibrary.ModelTierComplex}
	oneShot := estimateCostBand(&materializedBundle{
		Project:        &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: false}},
		RoleModelTiers: tiers,
	})
	daily := estimateCostBand(&materializedBundle{
		Project:        &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "24h"}},
		RoleModelTiers: tiers,
	})
	if oneShot == daily {
		t.Fatalf("expected the one-shot and daily-schedule bands to differ, both were %q", oneShot)
	}
}

func TestPerRoleCostPerRunUSD_AllTiers(t *testing.T) {
	cases := []struct {
		tier string
		want float64
	}{
		{rolelibrary.ModelTierTrivial, trivialTierCostPerRunUSD},
		{rolelibrary.ModelTierStandard, standardTierCostPerRunUSD},
		{rolelibrary.ModelTierComplex, complexTierCostPerRunUSD},
		{"unknown-tier", standardTierCostPerRunUSD},
		{"", standardTierCostPerRunUSD},
	}
	for _, c := range cases {
		if got := perRoleCostPerRunUSD(c.tier); got != c.want {
			t.Errorf("perRoleCostPerRunUSD(%q) = %v, want %v", c.tier, got, c.want)
		}
	}
}

func TestEstimateCostBand_UnknownTier_FallsBackToStandard(t *testing.T) {
	mb := &materializedBundle{
		Project:        &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: false}},
		RoleModelTiers: map[string]string{"mystery-role": "some-unrecognised-tier"},
	}
	got := estimateCostBand(mb)
	want := formatCostBand(standardTierCostPerRunUSD, standardTierCostPerRunUSD, true)
	if got != want {
		t.Errorf("expected an unknown tier to fall back to the standard band, got %q want %q", got, want)
	}
}

func TestEstimateCostBand_UnparseableCadence_FallsBackToOneShot(t *testing.T) {
	mb := &materializedBundle{
		Project:        &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "not-a-duration"}},
		RoleModelTiers: map[string]string{"researcher": rolelibrary.ModelTierStandard},
	}
	got := estimateCostBand(mb)
	if !strings.Contains(got, "one-time run") {
		t.Errorf("expected an unparseable cadence to fail safe to one-shot framing, got %q", got)
	}
}

// TestConverse_TierThree_CostBandIsGroundedAndOverridesLLM is the
// end-to-end seam: the LLM's own free-text cost_band
// (validComposedBundle() ships "~$0.10") must be OVERWRITTEN by the
// deterministic estimate before the envelope/session ever see it.
func TestConverse_TierThree_CostBandIsGroundedAndOverridesLLM(t *testing.T) {
	bundle := validComposedBundle()
	if bundle.Plan.CostBand != "~$0.10" {
		t.Fatalf("fixture drifted: expected the LLM free-text placeholder, got %q", bundle.Plan.CostBand)
	}
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", unrelatedDescription)
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Bundle == nil {
		t.Fatal("expected an accepted tier-3 bundle")
	}
	if res.Envelope.Bundle.Plan.CostBand == "~$0.10" {
		t.Error("expected the LLM's free-text cost_band to be overridden by the grounded estimate")
	}
	if !strings.Contains(res.Envelope.Bundle.Plan.CostBand, "estimate") {
		t.Errorf("expected the grounded cost_band to still read as an estimate, got %q", res.Envelope.Bundle.Plan.CostBand)
	}

	sess, err := store.Get(context.Background(), res.SessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(string(sess.Bundle), "~$0.10") {
		t.Error("expected the persisted session bundle to carry the grounded cost_band, not the LLM's original")
	}
}
