package executor

import (
	"testing"
	"time"
)

// These tests pin the invariant that fixes the 2026-06-18 IBKR "podman
// wait timed out" incident: a single LLM call must never be allowed to
// outlive the step that contains it. The agent default was 300s/call
// while review_risk's step budget was 240s, so one slow upstream call
// (observed 247s–564s) outlived the step and the container was killed
// mid-call. perCallTimeoutForStep couples the per-call timeout to the
// step's effective wall-clock so this can't happen by construction.

func TestLLMCallTimeoutForStep_AlwaysBelowStep(t *testing.T) {
	// The regression case: review_risk step = 4m, no operator-configured
	// ceiling. The result MUST be strictly below the step budget (and far
	// below the agent's old 300s default).
	step := 4 * time.Minute
	got := perCallTimeoutForStep(step, 0)
	if got <= 0 {
		t.Fatalf("expected a positive per-call timeout, got %v", got)
	}
	if got >= step {
		t.Fatalf("per-call timeout %v must be strictly below the step budget %v", got, step)
	}
	if got >= 300*time.Second {
		t.Fatalf("per-call timeout %v must be below the old 300s agent default", got)
	}
}

func TestLLMCallTimeoutForStep_ConfiguredCeilingWinsWhenTighter(t *testing.T) {
	// strategize step = 12m; operator pins agent_llm.timeout=120s. The
	// tighter operator ceiling must win.
	got := perCallTimeoutForStep(12*time.Minute, 120*time.Second)
	if got != 120*time.Second {
		t.Fatalf("expected the 120s configured ceiling, got %v", got)
	}
}

func TestLLMCallTimeoutForStep_StepFractionWinsWhenCeilingLooser(t *testing.T) {
	// A 2m step with a loose 300s ceiling: the step-derived cap must win
	// and stay below the step.
	step := 2 * time.Minute
	got := perCallTimeoutForStep(step, 300*time.Second)
	if got >= step {
		t.Fatalf("per-call timeout %v must be below the step budget %v", got, step)
	}
	if got != time.Duration(float64(step)*perCallStepTimeoutFraction) {
		t.Fatalf("expected step*%.2f = %v, got %v", perCallStepTimeoutFraction, time.Duration(float64(step)*perCallStepTimeoutFraction), got)
	}
}

func TestLLMCallTimeoutForStep_FloorButStillBelowStep(t *testing.T) {
	// A tiny step must not produce an absurdly small or zero timeout, and
	// must still stay strictly below the step.
	step := 30 * time.Second
	got := perCallTimeoutForStep(step, 0)
	if got < perCallTimeoutFloor && got != step-time.Second {
		t.Fatalf("expected at least the floor %v (or the step-1s clamp), got %v", perCallTimeoutFloor, got)
	}
	if got >= step {
		t.Fatalf("per-call timeout %v must stay below the step %v even after flooring", got, step)
	}
}

func TestLLMCallTimeoutForStep_NoStepFallsBackToCeiling(t *testing.T) {
	// When there is no per-step bound (stepTimeout<=0), fall back to the
	// operator ceiling verbatim.
	if got := perCallTimeoutForStep(0, 90*time.Second); got != 90*time.Second {
		t.Fatalf("expected the 90s ceiling, got %v", got)
	}
	// And with neither a step nor a ceiling, there is nothing to inject.
	if got := perCallTimeoutForStep(0, 0); got != 0 {
		t.Fatalf("expected 0 (nothing to inject), got %v", got)
	}
}

func TestPerCallTimeoutForStep_ShellDefaultCeilingCoupledBelowStep(t *testing.T) {
	// run_shell uses defaultAgentShellTimeout (300s) as its ceiling — the
	// uncoupled latent sibling of the IBKR LLM bug. On a step smaller than
	// 2x the ceiling the step fraction must win and stay below the step; on
	// a large step the 300s ceiling caps it (behaviour only tightens, never
	// loosens past today's default).
	if got := perCallTimeoutForStep(4*time.Minute, defaultAgentShellTimeout); got != 2*time.Minute {
		t.Fatalf("expected 120s (0.5 x 240s step), got %v", got)
	}
	if got := perCallTimeoutForStep(20*time.Minute, defaultAgentShellTimeout); got != defaultAgentShellTimeout {
		t.Fatalf("expected the 300s shell ceiling on a large step, got %v", got)
	}
}
