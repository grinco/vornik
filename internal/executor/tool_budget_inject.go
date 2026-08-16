package executor

import (
	"encoding/json"
	"strconv"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/toolbudget"
)

// applyStepTimeoutBudget scales a step's native wall-clock by the dynamic
// tool-budget complexity factor, mirroring resolveRoleToolBudget's scaling of
// the iteration budget so the two move together. Pure function.
//
// Exemptions (return native unchanged):
//   - feature disabled (cfg.Enabled == false),
//   - role not resolved (nil — findSwarmRole error), or
//   - a warm-pool role: warm roles run on their STATIC iteration budget
//     because the extraEnv override only reaches the ephemeral path (LLD §8),
//     so scaling their time DOWN while iterations stay full would manufacture
//     timeouts. Keep both static for them.
//
// Callers MUST apply this BEFORE the counterfactual step-timeout cap so the
// "override only lowers" invariant holds relative to the scaled native.
func applyStepTimeoutBudget(native time.Duration, roleConfig *registry.SwarmRole, tier string, autonomous bool, cfg toolbudget.Config) time.Duration {
	if !cfg.Enabled || roleConfig == nil || roleConfig.RuntimePolicy == "warm" {
		return native
	}
	return toolbudget.ScaleTimeout(native, toolbudget.Tier(tier), autonomous, cfg)
}

// perCallStepTimeoutFraction caps a single in-agent tool call (one LLM
// round-trip or one run_shell command) at this fraction of its step's
// effective wall-clock, leaving headroom for the step to observe the
// timeout, fail fast, and let the executor retry — rather than one call
// consuming the whole step.
const perCallStepTimeoutFraction = 0.5

// perCallTimeoutFloor keeps the coupled per-call timeout from collapsing
// to an absurdly small value on a tiny step, so legitimate calls aren't
// starved.
const perCallTimeoutFloor = 20 * time.Second

// defaultAgentShellTimeout mirrors the agent entrypoint's default for
// VORNIK_SHELL_TIMEOUT (images/vornik-agent/entrypoint.sh:
// "${VORNIK_SHELL_TIMEOUT:-300}", enforced via `timeout "$SHELL_TIMEOUT"`).
// Used as the ceiling when coupling the per-step shell timeout, so the
// behaviour only ever TIGHTENS below a small step — never loosens.
const defaultAgentShellTimeout = 300 * time.Second

// perCallTimeoutForStep derives the per-call timeout to inject for an
// in-agent tool call (LLM round-trip via VORNIK_LLM_TIMEOUT, or run_shell
// via VORNIK_SHELL_TIMEOUT) for a step whose effective wall-clock is
// stepTimeout, given an optional ceiling (the operator's configured value
// or the agent's documented default; <=0 means unset).
//
// INVARIANT: a single tool call must never be allowed to outlive the step
// that contains it. The 2026-06-18 IBKR failures were exactly this — the
// agent's 300s/call LLM default exceeded review_risk's 240s step, so one
// slow upstream call outlived the step and the container was killed
// mid-call by the podman-wait timeout (no clean failure, no retry). The
// uncoupled 300s VORNIK_SHELL_TIMEOUT is the same latent hazard. Coupling
// the per-call timeout to the step makes this impossible by construction,
// for every workflow, regardless of config.
//
// Rules: cap at perCallStepTimeoutFraction of the step, never above the
// ceiling, never below perCallTimeoutFloor, and always strictly below the
// step. When stepTimeout<=0 (no per-step bound) fall back to the ceiling
// verbatim; with neither, return 0 (inject nothing — the agent keeps its
// own default).
func perCallTimeoutForStep(stepTimeout, configuredCeiling time.Duration) time.Duration {
	out := configuredCeiling
	if stepTimeout > 0 {
		capped := time.Duration(float64(stepTimeout) * perCallStepTimeoutFraction)
		if out <= 0 || capped < out {
			out = capped
		}
	}
	if out <= 0 {
		return 0
	}
	if out < perCallTimeoutFloor {
		out = perCallTimeoutFloor
	}
	// The floor (or a loose ceiling on a tiny step) must never push the
	// per-call timeout up to or past the step itself.
	if stepTimeout > 0 && out >= stepTimeout {
		out = stepTimeout - time.Second
	}
	return out
}

// defaultAgentToolIterations mirrors the agent entrypoint's default for
// VORNIK_MAX_TOOL_ITERATIONS (images/vornik-agent/entrypoint.sh:
// "${VORNIK_MAX_TOOL_ITERATIONS:-30}"). Used as the base when a role pins no
// explicit budget, so scaling stays meaningful for un-tuned roles.
const defaultAgentToolIterations = 30

// roleToolBudgetBase returns the role's configured VORNIK_MAX_TOOL_ITERATIONS,
// falling back to defaultAgentToolIterations when the role pins none or the
// value doesn't parse.
func roleToolBudgetBase(roleConfig *registry.SwarmRole) int {
	if roleConfig != nil && roleConfig.Runtime.EnvVars != nil {
		if raw := roleConfig.Runtime.EnvVars["VORNIK_MAX_TOOL_ITERATIONS"]; raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				return n
			}
		}
	}
	return defaultAgentToolIterations
}

// validComplexityTier returns s if it is a recognised tier, else "". Producers
// pass the planner's raw verdict through this before writing it to execution
// state so a garbage value never persists; "" is read downstream as standard.
func validComplexityTier(s string) string {
	switch toolbudget.Tier(s) {
	case toolbudget.TierTrivial, toolbudget.TierStandard, toolbudget.TierComplex, toolbudget.TierOpenEnded:
		return s
	default:
		return ""
	}
}

// extractComplexityFromResult pulls a complexity verdict out of a step's
// result.json. It recognises the dev-pipeline analyst's nested
// `analysis.complexity` and a top-level `complexity` (nested wins), returning
// the canonical tier or "" when absent/invalid. This is the agent-step
// producer path (the lead-driven path reads LeadOutcome.Complexity directly);
// see https://docs.vornik.io §5.2.
func extractComplexityFromResult(resultBytes []byte) string {
	if len(resultBytes) == 0 {
		return ""
	}
	var parsed struct {
		Complexity string `json:"complexity"`
		Analysis   struct {
			Complexity string `json:"complexity"`
		} `json:"analysis"`
	}
	if err := json.Unmarshal(resultBytes, &parsed); err != nil {
		return ""
	}
	if t := validComplexityTier(parsed.Analysis.Complexity); t != "" {
		return t
	}
	return validComplexityTier(parsed.Complexity)
}

// resolveRoleToolBudget computes the effective tool-iteration budget for a
// worker spawn. inject is false when the feature is disabled — the caller
// then leaves the role's static env untouched (today's behaviour). When
// enabled, the caller injects effective as VORNIK_MAX_TOOL_ITERATIONS via
// extraEnv. See https://docs.vornik.io §6/§8.
func resolveRoleToolBudget(roleConfig *registry.SwarmRole, tier toolbudget.Tier, autonomous bool, cfg toolbudget.Config) (effective int, inject bool) {
	if !cfg.Enabled {
		return 0, false
	}
	// Warm roles run on their STATIC iteration budget, exactly as
	// applyStepTimeoutBudget keeps their timeout static. This MUST mirror that
	// guard: when a warm role's pool is exhausted it falls through to the
	// ephemeral spawn path (container.go) which would otherwise inject a
	// SCALED iteration budget — but the step timeout was already left native
	// (un-scaled) for the warm role. Scaling iterations UP (complex/open_ended)
	// against an un-scaled timeout manufactures the mid-work container kill the
	// timeout/iteration coupling exists to prevent. Keep both static for warm.
	if roleConfig != nil && roleConfig.RuntimePolicy == "warm" {
		return 0, false
	}
	base := roleToolBudgetBase(roleConfig)
	return toolbudget.Resolve(base, tier, autonomous, cfg), true
}

// fallbackTimeoutFactor and fallbackTimeoutFloor size a model-fallback
// attempt's step budget.
//
// WHY A FALLBACK NEEDS ITS OWN NUMBER. A step's declared timeout describes its
// PRIMARY. The fallback is, by construction, a different model — usually on a
// different provider, cold, and larger. Handing it the primary's budget sets
// the failover up to lose.
//
// Measured on 2026-08-16: configs/workflows/adaptive.md declares 30s for the
// route step, applyStepTimeoutBudget scaled it to 46s, Ollama Cloud 429'd its
// weekly limit eight times, the health breaker opened on glm-5.2, and the
// failover to zai.glm-5 died on "podman wait timed out after 46s". With
// perCallStepTimeoutFraction the LLM call inside it had ~23s for a 15KB prompt
// on Bedrock. Two executions failed this way in four minutes
// (exec_20260816163416_c5bac4bb8bc1196e, exec_20260816163619_a23b35f3b3349f30).
//
// The factor doubles; the floor keeps a small step from producing a fallback
// budget no cold cross-provider call could meet. 90s leaves ~45s for the LLM
// call after the per-call fraction, which is the scale the observed Bedrock
// call needed.
//
// This is NOT the perCallTimeoutForStep invariant, which stops a single call
// outliving its step and is correct. Tightening per-call time makes this
// failure worse, not better.
const (
	fallbackTimeoutFactor = 2
	fallbackTimeoutFloor  = 90 * time.Second
)

// fallbackStepTimeout returns the wall-clock budget for a model-fallback
// attempt, given the primary's.
//
// Guarantees the fallback never gets LESS time than the primary that just
// failed. A zero primary means the caller imposes no per-step bound, and is
// passed through unchanged: inventing a bound here would tighten a step that
// deliberately had none.
func fallbackStepTimeout(primary time.Duration) time.Duration {
	if primary <= 0 {
		return 0
	}
	scaled := primary * fallbackTimeoutFactor
	if scaled < fallbackTimeoutFloor {
		return fallbackTimeoutFloor
	}
	return scaled
}
