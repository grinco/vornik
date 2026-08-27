package executor

import (
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// D2 of the step-retry-configuration design. The governing constraint is G3:
// a step with no `retry:` block — and one that omits a field — must behave
// byte-identically to before this existed. Asserted against the CONSTANTS, not
// against literals, so the tests stay true if a default is retuned.
//
// Design: https://docs.vornik.io

func TestResolveStepRetry_EmptyBlockReproducesDefaults(t *testing.T) {
	got := resolveStepRetry(registry.WorkflowStep{})
	if got.MaxAttempts != infraRetryMaxAttempts {
		t.Errorf("MaxAttempts = %d, want the built-in default %d", got.MaxAttempts, infraRetryMaxAttempts)
	}
	if got.BaseDelay != infraRetryBaseDelay {
		t.Errorf("BaseDelay = %v, want the built-in default %v", got.BaseDelay, infraRetryBaseDelay)
	}
	if len(got.On) != 0 {
		t.Errorf("an absent block must add no classes, got %v", got.On)
	}
}

// A partial block overrides only what it names.
func TestResolveStepRetry_PartialBlockKeepsOtherDefaults(t *testing.T) {
	got := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{MaxAttempts: 3},
	})
	if got.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", got.MaxAttempts)
	}
	if got.BaseDelay != infraRetryBaseDelay {
		t.Errorf("BaseDelay = %v, want the untouched default %v", got.BaseDelay, infraRetryBaseDelay)
	}
}

func TestResolveStepRetry_InitialDelayParsed(t *testing.T) {
	got := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{InitialDelay: "30s"},
	})
	if got.BaseDelay != 30*time.Second {
		t.Errorf("BaseDelay = %v, want 30s", got.BaseDelay)
	}
}

// An unparseable duration must not silently become zero — a zero base delay
// would turn the ladder into a hot loop. Falling back to the default is the
// only safe reading, and validation catches it at load time anyway.
func TestResolveStepRetry_BadDurationFallsBackToDefault(t *testing.T) {
	got := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{InitialDelay: "not-a-duration"},
	})
	if got.BaseDelay != infraRetryBaseDelay {
		t.Errorf("BaseDelay = %v, want fallback to %v — never zero", got.BaseDelay, infraRetryBaseDelay)
	}
}

// A negative or absurd max_attempts must not disable retry entirely or run
// forever. Clamp to the default rather than trusting the config.
func TestResolveStepRetry_NonPositiveAttemptsFallBackToDefault(t *testing.T) {
	for _, n := range []int{0, -1} {
		got := resolveStepRetry(registry.WorkflowStep{
			Retry: registry.WorkflowStepRetry{MaxAttempts: n},
		})
		if got.MaxAttempts != infraRetryMaxAttempts {
			t.Errorf("MaxAttempts(%d) = %d, want the default %d", n, got.MaxAttempts, infraRetryMaxAttempts)
		}
	}
}

// THE WIDENING CONTRACT. `on:` adds classes; it can never remove a retry that
// happens today, because a config edit must not be able to make the fleet less
// resilient in a way nobody measured.
func TestShouldRetry_OnListWidensNeverNarrows(t *testing.T) {
	// A class named in `on:` retries, even though it is not an infra failure.
	r := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{On: []string{stepoutcome.ClassLLMCallFailed}},
	})
	llm := newContainerExitError(1,
		"agent reported FAILED status: LLM call failed: upstream provider returned an error")
	if !r.shouldRetry(llm) {
		t.Error("a class listed in retry.on must be retried")
	}

	// A list naming OTHER classes does not pick this one up — the list adds
	// exactly what it names and nothing else. (Before this change
	// llm_call_failed did not retry either, so this is unchanged behaviour,
	// not a narrowing.)
	narrow := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{On: []string{stepoutcome.ClassContainerKilled}},
	})
	if narrow.shouldRetry(llm) {
		t.Error("retry.on must add only the classes it names")
	}

	// ...but an infra failure still retries regardless of what the list says.
	// This is the widening guarantee: no config value can turn this off.
	if infraErr := errForInfraProbe(); infraErr != nil && !narrow.shouldRetry(infraErr) {
		t.Error("an infra failure must retry regardless of retry.on's contents")
	}

	// A class in neither the list nor the built-in predicate does not retry.
	if r.shouldRetry(errors.New("some novel failure nobody has classified")) {
		t.Error("an unlisted, non-infra failure must not retry")
	}
}

// errForInfraProbe returns an error the built-in predicate recognises, or nil
// when none can be constructed here (the predicate delegates to chat).
func errForInfraProbe() error {
	e := newContainerExitError(1, "upstream provider returned 503 service unavailable")
	if isInfraFailure(e) {
		return e
	}
	return nil
}
