package runtime

import "testing"

// TestPreallocCapsClampPathologicalSizes is the regression guard for the
// CodeQL go/uncontrolled-allocation-size findings on the podman-argv and
// warm-container env-var pre-allocations (StartContainer / warm pool). Those
// capacity hints derive from counts that are influenced by task-submitted
// container config, so each make-site clamps with min(n, cap). The clamp
// must bound a pathological count while leaving legitimate (small) counts
// untouched.
func TestPreallocCapsClampPathologicalSizes(t *testing.T) {
	const huge = 1 << 30

	if maxPodmanArgs <= 0 || maxWarmEnvVars <= 0 {
		t.Fatalf("prealloc caps must be positive: maxPodmanArgs=%d maxWarmEnvVars=%d", maxPodmanArgs, maxWarmEnvVars)
	}

	if got := min(huge, maxPodmanArgs); got != maxPodmanArgs {
		t.Errorf("podman argv prealloc: min(%d, %d) = %d, want clamp to %d", huge, maxPodmanArgs, got, maxPodmanArgs)
	}
	if got := min(5, maxPodmanArgs); got != 5 {
		t.Errorf("podman argv prealloc: legitimate arg count 5 must pass through, got %d", got)
	}

	if got := min(huge, maxWarmEnvVars); got != maxWarmEnvVars {
		t.Errorf("warm env prealloc: min(%d, %d) = %d, want clamp to %d", huge, maxWarmEnvVars, got, maxWarmEnvVars)
	}
	if got := min(8, maxWarmEnvVars); got != 8 {
		t.Errorf("warm env prealloc: legitimate env count 8 must pass through, got %d", got)
	}
}
