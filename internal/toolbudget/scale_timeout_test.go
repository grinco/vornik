package toolbudget

import (
	"testing"
	"time"
)

// ScaleTimeout couples a step's time budget to the same complexity factor and
// caps that Resolve applies to its iteration budget, so the two move together
// (https://docs.vornik.io). Pure function.

// timeoutCfg mirrors the recalibrated LLD factor block with a floor.
func timeoutCfg() Config {
	c := defaultCfg()
	c.Factors = map[Tier]float64{TierTrivial: 0.25, TierStandard: 0.5, TierComplex: 1.0, TierOpenEnded: 2.0}
	c.MaxFactor = 2.0
	c.AutonomyMaxFactor = 1.5
	c.MinStepTimeout = 5 * time.Minute
	return c
}

func TestScaleTimeout_ScalesByTierFactor(t *testing.T) {
	cfg := timeoutCfg()
	base := 45 * time.Minute
	cases := []struct {
		tier Tier
		want time.Duration
	}{
		{TierTrivial, 11*time.Minute + 15*time.Second},  // 45m * 0.25
		{TierStandard, 22*time.Minute + 30*time.Second}, // 45m * 0.5
		{TierComplex, 45 * time.Minute},                 // 45m * 1.0
		{TierOpenEnded, 90 * time.Minute},               // 45m * 2.0 (operator, max_factor 2.0)
	}
	for _, c := range cases {
		if got := ScaleTimeout(base, c.tier, false, cfg); got != c.want {
			t.Errorf("ScaleTimeout(45m, %q, operator) = %s; want %s", c.tier, got, c.want)
		}
	}
}

func TestScaleTimeout_AutonomyCeiling(t *testing.T) {
	cfg := timeoutCfg()
	// open_ended factor 2.0 is clamped to autonomy_max_factor 1.5 for
	// unattended tasks: 45m * 1.5 = 67.5m.
	got := ScaleTimeout(45*time.Minute, TierOpenEnded, true, cfg)
	want := 67*time.Minute + 30*time.Second
	if got != want {
		t.Errorf("ScaleTimeout(45m, open_ended, autonomous) = %s; want %s", got, want)
	}
}

func TestScaleTimeout_DisabledIsPassthrough(t *testing.T) {
	cfg := timeoutCfg()
	cfg.Enabled = false
	if got := ScaleTimeout(45*time.Minute, TierTrivial, false, cfg); got != 45*time.Minute {
		t.Errorf("disabled ScaleTimeout = %s; want unchanged 45m", got)
	}
}

func TestScaleTimeout_UnknownTierIsUnscaled(t *testing.T) {
	cfg := timeoutCfg()
	// "" / unrecognised → 1.0x (no scaling): the step keeps its native timeout.
	// Absent verdicts must NOT be silently downscaled (incident 2026-06-13).
	for _, tier := range []Tier{"", Tier("garbage")} {
		if got := ScaleTimeout(45*time.Minute, tier, false, cfg); got != 45*time.Minute {
			t.Errorf("ScaleTimeout(%q) = %s; want unchanged 45m (no silent downscale)", tier, got)
		}
	}
}

func TestScaleTimeout_Floor(t *testing.T) {
	cfg := timeoutCfg() // floor 5m
	// A downscale that lands below the floor is raised to the floor:
	// 10m * 0.25 = 2.5m → floored to 5m.
	if got := ScaleTimeout(10*time.Minute, TierTrivial, false, cfg); got != 5*time.Minute {
		t.Errorf("ScaleTimeout(10m, trivial) = %s; want floor 5m", got)
	}
	// The floor never raises a step ABOVE its native base: a 1m native step
	// floored against a 5m floor stays at 1m (the operator's explicit short
	// timeout wins over the floor).
	if got := ScaleTimeout(1*time.Minute, TierTrivial, false, cfg); got != 1*time.Minute {
		t.Errorf("ScaleTimeout(1m, trivial) = %s; want native 1m (floor capped at base)", got)
	}
	// A comfortable downscale stays untouched: 45m * 0.25 = 11.25m > 5m floor.
	if got := ScaleTimeout(45*time.Minute, TierTrivial, false, cfg); got != 11*time.Minute+15*time.Second {
		t.Errorf("ScaleTimeout(45m, trivial) = %s; want 11m15s (above floor)", got)
	}
}

func TestScaleTimeout_ZeroBase(t *testing.T) {
	cfg := timeoutCfg()
	if got := ScaleTimeout(0, TierOpenEnded, false, cfg); got != 0 {
		t.Errorf("ScaleTimeout(0, ...) = %s; want 0 (no base, nothing to scale)", got)
	}
}
