package executor

import (
	"testing"
	"time"
)

// Regression 2026-08-16: exec_20260816163416_c5bac4bb8bc1196e and
// exec_20260816163619_a23b35f3b3349f30 both died on
//
//	podman wait timed out after 46s: context deadline exceeded
//	(primary model "glm-5.2" also failed: MODEL_UNHEALTHY: circuit open)
//
// Ollama Cloud returned 429 weekly-usage-limit eight times, the agent-LLM
// health breaker opened on glm-5.2 exactly as designed, and the step failed
// over to zai.glm-5 — which inherited the primary's budget verbatim.
//
// configs/workflows/adaptive.md declares timeout 30s for the route step,
// scaled to 46s. That number describes the PRIMARY: one routing call from a
// fast hosted model. The fallback is a different provider, cold, and a larger
// model, and perCallTimeoutForStep then hands its LLM call half of that —
// about 23s for a 15KB prompt. The failover worked and could not succeed.
//
// A breaker that opens onto a budget the fallback cannot meet converts a
// recoverable outage into a task failure.
func TestFallbackStepTimeout(t *testing.T) {
	tests := []struct {
		name    string
		primary time.Duration
		want    time.Duration
		why     string
	}{
		{
			name:    "the route step that actually failed",
			primary: 46 * time.Second,
			want:    92 * time.Second,
			why:     "46s doubled is 92s, just above the 90s floor, so the factor governs and the floor does not bind",
		},
		{
			name:    "a tiny step still gets a workable fallback budget",
			primary: 10 * time.Second,
			want:    fallbackTimeoutFloor,
			why:     "doubling 10s gives 20s, which no cold cross-provider call can meet; the floor carries it",
		},
		{
			name:    "a generous step doubles",
			primary: 5 * time.Minute,
			want:    10 * time.Minute,
			why:     "well above the floor, so the factor governs",
		},
		{
			name:    "no declared timeout stays undeclared",
			primary: 0,
			want:    0,
			why:     "0 means the caller imposes no per-step bound; inventing one here would tighten a step that had none",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fallbackStepTimeout(tc.primary)
			if got != tc.want {
				t.Errorf("fallbackStepTimeout(%s) = %s, want %s — %s", tc.primary, got, tc.want, tc.why)
			}
		})
	}
}

// The invariant that matters: a fallback attempt must never get LESS time than
// the primary that just failed. Anything else means the failover is set up to
// lose.
func TestFallbackStepTimeout_NeverShrinks(t *testing.T) {
	for _, primary := range []time.Duration{
		1 * time.Second, 30 * time.Second, 46 * time.Second,
		2 * time.Minute, 6 * time.Minute, 1 * time.Hour,
	} {
		if got := fallbackStepTimeout(primary); got < primary {
			t.Errorf("fallbackStepTimeout(%s) = %s — a fallback must never get less time than the primary",
				primary, got)
		}
	}
}

// 46s was the observed failure. Pin that the fix actually changes that case,
// so a future edit to the factor or floor cannot silently restore it.
func TestFallbackStepTimeout_FixesTheObservedFailure(t *testing.T) {
	const observed = 46 * time.Second
	got := fallbackStepTimeout(observed)
	if got <= observed {
		t.Fatalf("fallback budget %s does not improve on the observed %s", got, observed)
	}
	// perCallTimeoutForStep gives an LLM call half its step. The 15KB-prompt
	// Bedrock call that failed had ~23s; it needs materially more.
	perCall := time.Duration(float64(got) * perCallStepTimeoutFraction)
	if perCall < 45*time.Second {
		t.Errorf("per-call budget for the fallback is %s — still too tight for a cold cross-provider call", perCall)
	}
}
