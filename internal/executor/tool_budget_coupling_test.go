package executor

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/toolbudget"
)

// These tests target applyStepTimeoutBudget (the executor-side step-timeout
// scaler, which has no direct coverage in tool_budget_inject_test.go) and the
// load-bearing invariant that the iteration budget (resolveRoleToolBudget) and
// the wall-clock budget (applyStepTimeoutBudget) scale TOGETHER. They also fill
// boundary gaps (factor exactly at a cap, tier=="" == standard-anchor) that the
// existing tests don't pin. Prefix: TestTB.

// tbTimeoutCfg mirrors the recalibrated LLD factor block used for step-timeout
// coupling (complex == 1.0x anchor) with both caps engaged. Distinct from the
// executor test's enabledBudgetCfg (which anchors complex at 1.5x); using the
// timeout-calibrated block here keeps the coupling assertions exact.
func tbTimeoutCfg() toolbudget.Config {
	return toolbudget.Config{
		Enabled:           true,
		Factors:           map[toolbudget.Tier]float64{toolbudget.TierTrivial: 0.25, toolbudget.TierStandard: 0.5, toolbudget.TierComplex: 1.0, toolbudget.TierOpenEnded: 2.0},
		MaxFactor:         2.0,
		AutonomyMaxFactor: 1.5,
	}
}

func tbRole(policy, budget string) *registry.SwarmRole {
	r := &registry.SwarmRole{Name: "coder", RuntimePolicy: policy}
	if budget != "" {
		r.Runtime.EnvVars = map[string]string{"VORNIK_MAX_TOOL_ITERATIONS": budget}
	}
	return r
}

// ---- applyStepTimeoutBudget exemptions (return native unchanged) ----

func TestTB_ApplyStepTimeout_DisabledIsPassthrough(t *testing.T) {
	cfg := tbTimeoutCfg()
	cfg.Enabled = false
	native := 45 * time.Minute
	// Even a downscaling tier must leave the native timeout untouched when the
	// feature is off — the disabled passthrough.
	if got := applyStepTimeoutBudget(native, tbRole("ephemeral", "250"), "trivial", false, cfg); got != native {
		t.Errorf("disabled applyStepTimeoutBudget = %s; want native %s", got, native)
	}
}

func TestTB_ApplyStepTimeout_NilRoleIsPassthrough(t *testing.T) {
	cfg := tbTimeoutCfg()
	native := 45 * time.Minute
	// A nil roleConfig (findSwarmRole error) must not scale — guards against a
	// nil-deref AND against scaling against an unknown role.
	if got := applyStepTimeoutBudget(native, nil, "open_ended", false, cfg); got != native {
		t.Errorf("nil-role applyStepTimeoutBudget = %s; want native %s", got, native)
	}
}

func TestTB_ApplyStepTimeout_WarmRoleStaysStatic(t *testing.T) {
	cfg := tbTimeoutCfg()
	native := 45 * time.Minute
	// Warm role: the timeout MUST stay native even for an up-scaling tier
	// (open_ended 2.0x). Scaling a warm role's time while its iterations stay
	// static (per resolveRoleToolBudget) would manufacture timeouts. This is the
	// timeout half of the warm-role coupling guard.
	if got := applyStepTimeoutBudget(native, tbRole("warm", "250"), "open_ended", false, cfg); got != native {
		t.Errorf("warm-role applyStepTimeoutBudget = %s; want native %s (static)", got, native)
	}
	// Sanity: an ephemeral role with the SAME inputs DOES scale up, proving the
	// guard keys on the policy, not a blanket disable.
	if got := applyStepTimeoutBudget(native, tbRole("ephemeral", "250"), "open_ended", false, cfg); got != 90*time.Minute {
		t.Errorf("ephemeral open_ended applyStepTimeoutBudget = %s; want 90m (45m*2.0)", got)
	}
}

// ---- applyStepTimeoutBudget scaling by tier ----

func TestTB_ApplyStepTimeout_ScalesByTier(t *testing.T) {
	cfg := tbTimeoutCfg()
	base := 40 * time.Minute
	cases := []struct {
		tier string
		want time.Duration
	}{
		{"trivial", 10 * time.Minute},    // 40m * 0.25
		{"standard", 20 * time.Minute},   // 40m * 0.5
		{"complex", 40 * time.Minute},    // 40m * 1.0 (anchor)
		{"open_ended", 80 * time.Minute}, // 40m * 2.0 (operator, at max_factor)
	}
	for _, c := range cases {
		if got := applyStepTimeoutBudget(base, tbRole("ephemeral", "250"), c.tier, false, cfg); got != c.want {
			t.Errorf("applyStepTimeoutBudget(40m, %q, operator) = %s; want %s", c.tier, got, c.want)
		}
	}
}

func TestTB_ApplyStepTimeout_AutonomousTighterCeiling(t *testing.T) {
	cfg := tbTimeoutCfg()
	// open_ended factor 2.0 is clamped to autonomy_max_factor 1.5 for unattended
	// work: 40m * 1.5 = 60m (vs 80m for an operator task above).
	if got := applyStepTimeoutBudget(40*time.Minute, tbRole("ephemeral", "250"), "open_ended", true, cfg); got != 60*time.Minute {
		t.Errorf("autonomous open_ended timeout = %s; want 60m (capped 1.5x)", got)
	}
}

// TestTB_ApplyStepTimeout_EmptyTierIsStandardAnchor pins the boundary that an
// empty tier resolves to factor 1.0 (the native base), NOT a silent downscale —
// the incident-2026-06-13 invariant, asserted on the executor wrapper.
func TestTB_ApplyStepTimeout_EmptyTierUnscaled(t *testing.T) {
	cfg := tbTimeoutCfg()
	native := 33 * time.Minute
	for _, tier := range []string{"", "garbage", "Complex"} {
		if got := applyStepTimeoutBudget(native, tbRole("ephemeral", "250"), tier, false, cfg); got != native {
			t.Errorf("applyStepTimeoutBudget(33m, %q) = %s; want native %s (no silent downscale)", tier, got, native)
		}
	}
}

// ---- The load-bearing coupling: iterations and wall-clock scale together ----

// TestTB_Coupling_IterationsAndTimeoutMoveTogether is the core invariant: for an
// ephemeral role, the ratio applied to the iteration budget equals the ratio
// applied to the step timeout, across every tier. If these ever diverge a step
// can be granted iterations it has no wall-clock to spend (or vice-versa) — the
// container-kill class of incident the coupling exists to prevent.
func TestTB_Coupling_IterationsAndTimeoutMoveTogether(t *testing.T) {
	cfg := tbTimeoutCfg()
	const base = 200
	const nativeTimeout = 60 * time.Minute
	role := tbRole("ephemeral", "200")

	for _, tier := range []string{"trivial", "standard", "complex", "open_ended"} {
		for _, autonomous := range []bool{false, true} {
			eff, inject := resolveRoleToolBudget(role, toolbudget.Tier(tier), autonomous, cfg)
			if !inject {
				t.Fatalf("ephemeral role must inject for tier=%q autonomous=%v", tier, autonomous)
			}
			gotTimeout := applyStepTimeoutBudget(nativeTimeout, role, tier, autonomous, cfg)

			iterRatio := float64(eff) / float64(base)
			timeRatio := float64(gotTimeout) / float64(nativeTimeout)
			if iterRatio != timeRatio {
				t.Errorf("tier=%q autonomous=%v: iteration ratio %.3f != timeout ratio %.3f (coupling broken)",
					tier, autonomous, iterRatio, timeRatio)
			}
		}
	}
}

// TestTB_Coupling_WarmKeepsBothStatic asserts the warm exemption holds on BOTH
// budgets simultaneously: no iteration injection AND a native (unscaled)
// timeout, for the up-scaling tier where a mismatch would bite hardest.
func TestTB_Coupling_WarmKeepsBothStatic(t *testing.T) {
	cfg := tbTimeoutCfg()
	warm := tbRole("warm", "250")
	native := 45 * time.Minute

	_, inject := resolveRoleToolBudget(warm, toolbudget.TierOpenEnded, false, cfg)
	gotTimeout := applyStepTimeoutBudget(native, warm, "open_ended", false, cfg)

	if inject {
		t.Errorf("warm role: iteration budget must stay static (inject=false)")
	}
	if gotTimeout != native {
		t.Errorf("warm role: timeout must stay native %s, got %s", native, gotTimeout)
	}
}

// TestTB_Coupling_DisabledKeepsBothUntouched: when the feature is off, neither
// budget is altered — the full passthrough across the coupled pair.
func TestTB_Coupling_DisabledKeepsBothUntouched(t *testing.T) {
	cfg := tbTimeoutCfg()
	cfg.Enabled = false
	role := tbRole("ephemeral", "250")
	native := 45 * time.Minute

	eff, inject := resolveRoleToolBudget(role, toolbudget.TierComplex, false, cfg)
	gotTimeout := applyStepTimeoutBudget(native, role, "complex", false, cfg)

	if inject {
		t.Errorf("disabled: must not inject an iteration budget")
	}
	if eff != 0 {
		t.Errorf("disabled effective = %d; want 0 (caller leaves static env)", eff)
	}
	if gotTimeout != native {
		t.Errorf("disabled timeout = %s; want native %s", gotTimeout, native)
	}
}

// ---- Boundary: factor exactly AT a cap is not further clamped ----

func TestTB_ResolveRoleToolBudget_FactorExactlyAtMaxCap(t *testing.T) {
	cfg := tbTimeoutCfg() // open_ended 2.0 == MaxFactor 2.0
	// Operator open_ended sits EXACTLY at MaxFactor — the clamp uses `>` so it
	// must pass through unmodified: 100 * 2.0 = 200.
	eff, _ := resolveRoleToolBudget(tbRole("ephemeral", "100"), toolbudget.TierOpenEnded, false, cfg)
	if eff != 200 {
		t.Errorf("operator open_ended (factor==max) = %d; want 200 (100*2.0, not clamped)", eff)
	}
}

func TestTB_ResolveRoleToolBudget_FactorExactlyAtAutonomyCap(t *testing.T) {
	cfg := tbTimeoutCfg()
	cfg.Factors[toolbudget.TierComplex] = 1.5 // == AutonomyMaxFactor 1.5
	// Autonomous complex sits EXACTLY at AutonomyMaxFactor — not further clamped:
	// 100 * 1.5 = 150.
	eff, _ := resolveRoleToolBudget(tbRole("ephemeral", "100"), toolbudget.TierComplex, true, cfg)
	if eff != 150 {
		t.Errorf("autonomous complex (factor==autonomy cap) = %d; want 150 (not clamped)", eff)
	}
}

// TestTB_ResolveRoleToolBudget_FloorsAtOne guards the effective<1 clamp: a tiny
// base aggressively downscaled can't fall below 1 iteration (a 0-iteration agent
// could never call a tool).
func TestTB_ResolveRoleToolBudget_FloorsAtOne(t *testing.T) {
	cfg := tbTimeoutCfg() // trivial 0.25
	// base 2 * 0.25 = 0.5 → rounds to 0 → floored to 1.
	eff, inject := resolveRoleToolBudget(tbRole("ephemeral", "2"), toolbudget.TierTrivial, false, cfg)
	if !inject {
		t.Fatalf("must inject")
	}
	if eff != 1 {
		t.Errorf("tiny base downscaled = %d; want floor of 1", eff)
	}
}

// TestTB_ResolveRoleToolBudget_UnpinnedRoleUsesDefaultBase: a role that pins no
// budget scales the daemon default (30), not 0 — scaling stays meaningful for
// un-tuned roles. complex==1.0x so 30 stays 30; open_ended 2.0x → 60.
func TestTB_ResolveRoleToolBudget_UnpinnedRoleUsesDefaultBase(t *testing.T) {
	cfg := tbTimeoutCfg()
	role := tbRole("ephemeral", "") // no VORNIK_MAX_TOOL_ITERATIONS pinned
	if eff, _ := resolveRoleToolBudget(role, toolbudget.TierComplex, false, cfg); eff != defaultAgentToolIterations {
		t.Errorf("unpinned complex = %d; want default %d (1.0x)", eff, defaultAgentToolIterations)
	}
	if eff, _ := resolveRoleToolBudget(role, toolbudget.TierOpenEnded, false, cfg); eff != 2*defaultAgentToolIterations {
		t.Errorf("unpinned open_ended = %d; want %d (default*2.0)", eff, 2*defaultAgentToolIterations)
	}
}

// TestTB_RoleToolBudgetBase_RejectsNonPositive: a role pinning a non-positive
// value (0 or negative) is unparseable-equivalent and falls back to the default,
// since Atoi("0")>0 is false and a negative budget is nonsensical.
func TestTB_RoleToolBudgetBase_RejectsNonPositive(t *testing.T) {
	for _, v := range []string{"0", "-5"} {
		if got := roleToolBudgetBase(tbRole("ephemeral", v)); got != defaultAgentToolIterations {
			t.Errorf("roleToolBudgetBase(%q) = %d; want default %d", v, got, defaultAgentToolIterations)
		}
	}
}

// TestTB_RoleToolBudgetBase_NilRoleAndNilEnv: defensive guards — a nil role, and
// a non-nil role with nil EnvVars map, both fall back to the default without a
// nil-deref.
func TestTB_RoleToolBudgetBase_NilRoleAndNilEnv(t *testing.T) {
	if got := roleToolBudgetBase(nil); got != defaultAgentToolIterations {
		t.Errorf("roleToolBudgetBase(nil) = %d; want default %d", got, defaultAgentToolIterations)
	}
	if got := roleToolBudgetBase(&registry.SwarmRole{Name: "x"}); got != defaultAgentToolIterations {
		t.Errorf("roleToolBudgetBase(nil-env) = %d; want default %d", got, defaultAgentToolIterations)
	}
}

// TestTB_ExtractComplexity_TopLevelWhenNestedInvalid pins a precedence subtlety
// the existing table doesn't: a nested analysis.complexity that is INVALID must
// not block a VALID top-level complexity — extraction falls through to top-level
// rather than returning "" on the bad nested value.
func TestTB_ExtractComplexity_TopLevelWhenNestedInvalid(t *testing.T) {
	body := `{"complexity":"open_ended","analysis":{"complexity":"bogus"}}`
	if got := extractComplexityFromResult([]byte(body)); got != "open_ended" {
		t.Errorf("extract(%s) = %q; want open_ended (fall through invalid nested)", body, got)
	}
}

// TestTB_ExtractComplexity_NilAndWhitespaceBytes: nil bytes behave like empty
// (=> ""), and a body whose only complexity value is whitespace is invalid.
func TestTB_ExtractComplexity_NilAndWhitespaceBytes(t *testing.T) {
	if got := extractComplexityFromResult(nil); got != "" {
		t.Errorf("extract(nil) = %q; want \"\"", got)
	}
	// validComplexityTier does not trim, so a quoted "  complex  " is rejected.
	body := `{"complexity":"  complex  "}`
	if got := extractComplexityFromResult([]byte(body)); got != "" {
		t.Errorf("extract(%s) = %q; want \"\" (untrimmed value not a valid tier)", body, got)
	}
}
