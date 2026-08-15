package executor

import (
	"slices"
	"testing"
	"time"
)

// A role that narrows to three tools still gets the universal read-only
// baseline: the allowlist bounds what an agent may DO, not what it may learn.
func TestWithAlwaysGrantedTools_AddsTheBaselineToANarrowedRole(t *testing.T) {
	got := withAlwaysGrantedTools([]string{"file_read", "run_shell"})

	for _, want := range []string{"file_read", "run_shell", "memory_search", "skill_fetch"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	// The role's own tools stay first: the entrypoint reads this list in order
	// and an operator reading a payload should see their declaration intact.
	if got[0] != "file_read" || got[1] != "run_shell" {
		t.Errorf("role's declared order was disturbed: %v", got)
	}
}

func TestWithAlwaysGrantedTools_DoesNotDuplicateADeclaredTool(t *testing.T) {
	got := withAlwaysGrantedTools([]string{"memory_search", "file_read"})

	n := 0
	for _, t2 := range got {
		if t2 == "memory_search" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("memory_search appears %d times in %v; a duplicated grant is a config smell "+
			"an operator will chase", n, got)
	}
}

func TestWithAlwaysGrantedTools_HandlesAnEmptyAllowlist(t *testing.T) {
	if got := withAlwaysGrantedTools(nil); len(got) != 2 {
		t.Errorf("got %v, want just the baseline", got)
	}
}

// The payload's config.timeoutSeconds is part of the agent runtime contract
// (LLD 09) and the fake agent enforces it. It was hardcoded to 30 minutes
// regardless of the step's real budget, so the contract advertised a number
// that was usually false — and once step timeouts scale, false in the direction
// that matters.
func TestAgentTimeoutSeconds_ReportsTheStepsRealBudget(t *testing.T) {
	got := agentTimeoutSeconds(&agentInputOpts{StepTimeout: 90 * time.Second})

	if got != 90 {
		t.Errorf("timeoutSeconds = %d, want the step's actual 90", got)
	}
}

// A caller that cannot supply the budget must behave exactly as before.
func TestAgentTimeoutSeconds_FallsBackWhenUnset(t *testing.T) {
	want := int(defaultAgentTimeout / time.Second)

	if got := agentTimeoutSeconds(&agentInputOpts{}); got != want {
		t.Errorf("unset = %d, want the %d fallback", got, want)
	}
	if got := agentTimeoutSeconds(nil); got != want {
		t.Errorf("nil opts = %d, want the %d fallback", got, want)
	}
}
