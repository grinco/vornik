package toolbudget

import (
	"testing"
	"time"
)

// The resolver is the heart of the dynamic-tool-budget feature
// (https://docs.vornik.io §6). It scales a
// role's configured VORNIK_MAX_TOOL_ITERATIONS by a tier factor, bounded by
// config caps, with autonomy held to a tighter ceiling. Pure function — no
// I/O — so every behaviour is a table case here.

// defaultCfg mirrors the LLD §7 default block with the feature enabled.
func defaultCfg() Config {
	return Config{
		Enabled:           true,
		Factors:           map[Tier]float64{TierTrivial: 0.5, TierStandard: 1.0, TierComplex: 1.5, TierOpenEnded: 2.5},
		MaxFactor:         2.5,
		AutonomyMaxFactor: 1.5,
	}
}

func TestResolve_ScalesByTierFactor(t *testing.T) {
	cfg := defaultCfg()
	cases := []struct {
		tier Tier
		want int
	}{
		{TierTrivial, 50},    // 100 * 0.5
		{TierStandard, 100},  // 100 * 1.0
		{TierComplex, 150},   // 100 * 1.5
		{TierOpenEnded, 250}, // 100 * 2.5
	}
	for _, c := range cases {
		if got := Resolve(100, c.tier, false, cfg); got != c.want {
			t.Errorf("Resolve(100, %q, operator) = %d; want %d", c.tier, got, c.want)
		}
	}
}

func TestResolve_DisabledIsPassthrough(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	// Even an open_ended tier must not change the base when disabled.
	if got := Resolve(250, TierOpenEnded, false, cfg); got != 250 {
		t.Errorf("disabled Resolve = %d; want 250 (passthrough)", got)
	}
}

func TestResolve_UnknownTierFallsBackToStandard(t *testing.T) {
	cfg := defaultCfg()
	if got := Resolve(100, Tier("garbage"), false, cfg); got != 100 {
		t.Errorf("unknown tier Resolve = %d; want 100 (standard 1.0x)", got)
	}
	if got := Resolve(100, Tier(""), false, cfg); got != 100 {
		t.Errorf("empty tier Resolve = %d; want 100 (standard 1.0x)", got)
	}
}

func TestResolve_AutonomyCapsBelowMaxFactor(t *testing.T) {
	cfg := defaultCfg()
	// open_ended would be 2.5x, but an autonomous task is capped at 1.5x.
	if got := Resolve(100, TierOpenEnded, true, cfg); got != 150 {
		t.Errorf("autonomous open_ended Resolve = %d; want 150 (capped at autonomy_max_factor 1.5)", got)
	}
	// complex (1.5x) is exactly at the autonomy ceiling — unchanged.
	if got := Resolve(100, TierComplex, true, cfg); got != 150 {
		t.Errorf("autonomous complex Resolve = %d; want 150", got)
	}
}

func TestResolve_MaxFactorCapsOperatorTasks(t *testing.T) {
	cfg := defaultCfg()
	cfg.Factors[TierOpenEnded] = 5.0 // misconfigured high factor
	// Operator task must still be capped at max_factor 2.5.
	if got := Resolve(100, TierOpenEnded, false, cfg); got != 250 {
		t.Errorf("operator open_ended Resolve = %d; want 250 (capped at max_factor 2.5)", got)
	}
}

func TestResolve_FloorOfOne(t *testing.T) {
	cfg := defaultCfg()
	cfg.Factors[TierTrivial] = 0.0 // pathological
	if got := Resolve(100, TierTrivial, false, cfg); got != 1 {
		t.Errorf("Resolve with 0 factor = %d; want 1 (floor)", got)
	}
}

func TestResolve_RoundsToNearest(t *testing.T) {
	cfg := defaultCfg()
	// 33 * 1.5 = 49.5 -> 50 (round half up, not truncate to 49).
	if got := Resolve(33, TierComplex, false, cfg); got != 50 {
		t.Errorf("Resolve(33, complex) = %d; want 50 (rounded)", got)
	}
}

// TestResolve_AbsentTierNoSilentDownscale is the regression guard for the
// 2026-06-13 incident: with the production-style table (standard=0.5), an ABSENT
// or unknown tier must resolve to the role base (1.0x), NOT be silently halved.
// Only an EXPLICIT trivial/standard verdict downscales. Iterations + timeout
// scale by the identical factor.
func TestResolve_AbsentTierNoSilentDownscale(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		Factors:        map[Tier]float64{TierTrivial: 0.25, TierStandard: 0.5, TierComplex: 1.0, TierOpenEnded: 2.0},
		MaxFactor:      2.0,
		MinStepTimeout: 5 * time.Minute,
	}
	base := 50
	baseTimeout := 15 * time.Minute

	// Absent / unknown → role base, full timeout (no silent downscale).
	for _, tier := range []Tier{"", Tier("garbage")} {
		if got := Resolve(base, tier, false, cfg); got != 50 {
			t.Errorf("Resolve(absent=%q) = %d; want 50 (role base, no downscale)", tier, got)
		}
		if got := ScaleTimeout(baseTimeout, tier, false, cfg); got != 15*time.Minute {
			t.Errorf("ScaleTimeout(absent=%q) = %s; want 15m (no downscale)", tier, got)
		}
	}
	// Explicit standard still downscales (the planner asked for it).
	if got := Resolve(base, TierStandard, false, cfg); got != 25 {
		t.Errorf("Resolve(standard) = %d; want 25 (0.5x)", got)
	}
	// Explicit open_ended scales up, coupling iterations + timeout.
	if got := Resolve(base, TierOpenEnded, false, cfg); got != 100 {
		t.Errorf("Resolve(open_ended) = %d; want 100 (2x)", got)
	}
	if got := ScaleTimeout(baseTimeout, TierOpenEnded, false, cfg); got != 30*time.Minute {
		t.Errorf("ScaleTimeout(open_ended) = %s; want 30m (2x)", got)
	}
}
