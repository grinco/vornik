package executor

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/toolbudget"
)

func enabledBudgetCfg() toolbudget.Config {
	return toolbudget.Config{
		Enabled:           true,
		Factors:           map[toolbudget.Tier]float64{toolbudget.TierTrivial: 0.5, toolbudget.TierStandard: 1.0, toolbudget.TierComplex: 1.5, toolbudget.TierOpenEnded: 2.5},
		MaxFactor:         2.5,
		AutonomyMaxFactor: 1.5,
	}
}

func roleWithBudget(v string) *registry.SwarmRole {
	r := &registry.SwarmRole{Name: "coder"}
	if v != "" {
		r.Runtime.EnvVars = map[string]string{"VORNIK_MAX_TOOL_ITERATIONS": v}
	}
	return r
}

func TestRoleToolBudgetBase_ReadsRoleEnv(t *testing.T) {
	if got := roleToolBudgetBase(roleWithBudget("250")); got != 250 {
		t.Errorf("base from env = %d; want 250", got)
	}
}

func TestRoleToolBudgetBase_DefaultsWhenUnset(t *testing.T) {
	if got := roleToolBudgetBase(roleWithBudget("")); got != defaultAgentToolIterations {
		t.Errorf("base unset = %d; want default %d", got, defaultAgentToolIterations)
	}
	if got := roleToolBudgetBase(roleWithBudget("notanint")); got != defaultAgentToolIterations {
		t.Errorf("base unparseable = %d; want default %d", got, defaultAgentToolIterations)
	}
}

func TestExtractComplexityFromResult(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"analyst nested", `{"analysis":{"feature":"x","complexity":"complex","ready":true}}`, "complex"},
		{"top-level", `{"complexity":"open_ended","message":"hi"}`, "open_ended"},
		{"nested wins when both", `{"complexity":"trivial","analysis":{"complexity":"complex"}}`, "complex"},
		{"absent", `{"analysis":{"feature":"x","ready":true}}`, ""},
		{"garbage tier ignored", `{"analysis":{"complexity":"huge"}}`, ""},
		{"invalid json", `not json`, ""},
		{"empty", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractComplexityFromResult([]byte(c.body)); got != c.want {
				t.Errorf("extractComplexityFromResult(%s) = %q; want %q", c.body, got, c.want)
			}
		})
	}
}

func TestResolveRoleToolBudget_DisabledDoesNotInject(t *testing.T) {
	cfg := enabledBudgetCfg()
	cfg.Enabled = false
	_, inject := resolveRoleToolBudget(roleWithBudget("250"), toolbudget.TierComplex, false, cfg)
	if inject {
		t.Errorf("disabled feature must not inject (leave static env)")
	}
}

func TestResolveRoleToolBudget_ScalesRoleBaseByTier(t *testing.T) {
	cfg := enabledBudgetCfg()
	// coder base 250, complex 1.5x (operator task) = 375.
	eff, inject := resolveRoleToolBudget(roleWithBudget("250"), toolbudget.TierComplex, false, cfg)
	if !inject {
		t.Fatalf("enabled feature must inject")
	}
	if eff != 375 {
		t.Errorf("effective = %d; want 375 (250 * 1.5)", eff)
	}
}

// TestResolveRoleToolBudget_WarmRoleStaysStatic guards the warm-fallback
// timeout-asymmetry fix. A warm role runs on its STATIC iteration budget
// (mirroring applyStepTimeoutBudget, which returns the native timeout for warm
// roles). Without this, a warm role whose pool is exhausted falls through to
// the ephemeral path and would get SCALED iterations against an UN-scaled
// (native) timeout — recreating the 30m container-kill incident the
// timeout/iteration coupling was designed to prevent.
func TestResolveRoleToolBudget_WarmRoleStaysStatic(t *testing.T) {
	cfg := enabledBudgetCfg()
	warm := roleWithBudget("250")
	warm.RuntimePolicy = "warm"
	_, inject := resolveRoleToolBudget(warm, toolbudget.TierOpenEnded, false, cfg)
	if inject {
		t.Errorf("warm role must NOT get a scaled iteration budget (stays static, like its timeout)")
	}
	// And an ephemeral role with the same inputs still scales — proving the
	// guard is keyed on the policy, not a blanket disable.
	eph := roleWithBudget("250")
	eph.RuntimePolicy = "ephemeral"
	if _, inject := resolveRoleToolBudget(eph, toolbudget.TierOpenEnded, false, cfg); !inject {
		t.Errorf("ephemeral role must still inject a scaled budget")
	}
}

func TestValidComplexityTier(t *testing.T) {
	for _, ok := range []string{"trivial", "standard", "complex", "open_ended"} {
		if got := validComplexityTier(ok); got != ok {
			t.Errorf("validComplexityTier(%q) = %q; want %q", ok, got, ok)
		}
	}
	// Unknown / empty / wrong-case → "" (caller leaves state at standard).
	for _, bad := range []string{"", "garbage", "Complex", "open-ended", "huge"} {
		if got := validComplexityTier(bad); got != "" {
			t.Errorf("validComplexityTier(%q) = %q; want \"\"", bad, got)
		}
	}
}

func TestResolveRoleToolBudget_AutonomyCaps(t *testing.T) {
	cfg := enabledBudgetCfg()
	// open_ended would be 2.5x=625, but autonomous caps at 1.5x=375.
	eff, _ := resolveRoleToolBudget(roleWithBudget("250"), toolbudget.TierOpenEnded, true, cfg)
	if eff != 375 {
		t.Errorf("autonomous open_ended = %d; want 375 (capped 1.5x)", eff)
	}
}

// TestToolBudget_EndToEnd_ExtraEnvOverridesStatic is the full-chain guard for
// the injection seam (https://docs.vornik.io §8):
// the resolved budget, carried via extraEnv, must override the role's static
// VORNIK_MAX_TOOL_ITERATIONS in the actual ContainerConfig the runtime
// receives. Mirrors TestExecutor_StartContainer_ModelOverride.
func TestToolBudget_EndToEnd_ExtraEnvOverridesStatic(t *testing.T) {
	e, rt, _, _, _ := setup()

	// coder role pins a static budget of 250 (as in dev-swarm.md).
	role := &registry.SwarmRole{
		Name: "coder",
		Runtime: registry.SwarmRoleRuntime{
			Image:   "fake-agent:latest",
			EnvVars: map[string]string{"VORNIK_MAX_TOOL_ITERATIONS": "250"},
		},
	}

	cfg := enabledBudgetCfg()

	// Operator task, complex tier → 250 * 1.5 = 375. This is exactly what
	// the executor computes at container.go's injection point.
	eff, inject := resolveRoleToolBudget(role, toolbudget.TierComplex, false, cfg)
	assert.True(t, inject)
	extraEnv := map[string]string{"VORNIK_MAX_TOOL_ITERATIONS": strconv.Itoa(eff)}

	_, err := e.startContainer(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, "e1", "fake-agent:latest",
		"coder", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, extraEnv)
	assert.NoError(t, err)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	// The scaled value reached the container, overriding the static 250.
	assert.Equal(t, "375", rt.lastConfig.EnvVars["VORNIK_MAX_TOOL_ITERATIONS"])
}

// TestToolBudget_EndToEnd_DisabledKeepsStatic proves the passthrough: when the
// feature is off, no extraEnv override is produced and the role's static
// budget reaches the container unchanged.
func TestToolBudget_EndToEnd_DisabledKeepsStatic(t *testing.T) {
	e, rt, _, _, _ := setup()
	role := &registry.SwarmRole{
		Name: "coder",
		Runtime: registry.SwarmRoleRuntime{
			Image:   "fake-agent:latest",
			EnvVars: map[string]string{"VORNIK_MAX_TOOL_ITERATIONS": "250"},
		},
	}

	cfg := enabledBudgetCfg()
	cfg.Enabled = false
	_, inject := resolveRoleToolBudget(role, toolbudget.TierOpenEnded, false, cfg)
	assert.False(t, inject, "disabled feature must not inject")

	// With no injection, extraEnv carries no budget override.
	_, err := e.startContainer(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"}, "e1", "fake-agent:latest",
		"coder", "/tmp/in", "/tmp/out", "/tmp/work", role, "", e.config.DefaultTimeout, nil)
	assert.NoError(t, err)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, "250", rt.lastConfig.EnvVars["VORNIK_MAX_TOOL_ITERATIONS"])
}
