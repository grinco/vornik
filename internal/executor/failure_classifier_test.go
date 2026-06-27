package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// TestClassifyExecutionFailure_HallucinatedPlacement pins the
// motivating class: the verifier's "hallucinated placement" detail
// classifies into HALLUCINATED_PLACEMENT rather than the generic
// TOOL_ERROR fall-through. Operators on the trading dashboard need
// this distinction to tell "the broker tool actually broke" from
// "the LLM invented the response without calling the broker".
func TestClassifyExecutionFailure_HallucinatedPlacement(t *testing.T) {
	// Real-world shape from the verifier — phase-2 wrapper plus the
	// claim-vs-audit detail.
	msg := `phase-2 verifier(s) failed: verifier "placements_match_audit" (placements_match_audit): hallucinated placement: result.json declares 1 placed entry but tool audit shows only 0 mcp__broker__place_order call(s) — the executor fabricated broker responses without invoking the broker tool`
	got := ClassifyExecutionFailure(nil, msg)
	assert.Equal(t, persistence.TaskFailureClassHallucinatedPlacement, got)
}

// TestClassifyExecutionFailure_HallucinatedPlacementBeatsToolError —
// the verifier message contains the substring "tool" (it names the
// broker MCP tool). Ensure the hallucination branch runs BEFORE the
// generic "tool ..." catch-all, otherwise the precise class is
// shadowed.
func TestClassifyExecutionFailure_HallucinatedPlacementBeatsToolError(t *testing.T) {
	msg := "tool audit mismatch — hallucinated placement: 2 claimed, 0 actual"
	got := ClassifyExecutionFailure(nil, msg)
	assert.Equal(t, persistence.TaskFailureClassHallucinatedPlacement, got)
}

// TestClassifyExecutionFailure_StructuralStdlibErrors covers the
// errors.Is(context.DeadlineExceeded / context.Canceled) branches
// that take priority over the string-matching cases. These are the
// most reliable signal — the classifier should never miss them.
func TestClassifyExecutionFailure_StructuralStdlibErrors(t *testing.T) {
	assert.Equal(t, persistence.TaskFailureClassTimeout,
		ClassifyExecutionFailure(context.DeadlineExceeded, ""))
	assert.Equal(t, persistence.TaskFailureClassCancelled,
		ClassifyExecutionFailure(context.Canceled, ""))
	// Even when the hint string would suggest a different class, the
	// structural error wins.
	assert.Equal(t, persistence.TaskFailureClassTimeout,
		ClassifyExecutionFailure(context.DeadlineExceeded, "rate limit hit"))
}

// TestClassifyExecutionFailure_EmptyInput — both nil err and empty
// hint must produce an empty class string. The caller defaults to
// "EXECUTION_ERROR" when this is empty; we shouldn't pre-empt that
// fallback by inventing a class.
func TestClassifyExecutionFailure_EmptyInput(t *testing.T) {
	assert.Equal(t, "", ClassifyExecutionFailure(nil, ""))
}

// TestClassifyExecutionFailure_StringBranches walks every
// containsAny branch in the classifier. Each case targets one
// branch; a refactor that re-orders the switch or drops a branch
// will surface here.
func TestClassifyExecutionFailure_StringBranches(t *testing.T) {
	cases := []struct {
		name   string
		hint   string
		expect string
	}{
		{"merge_failed", "could not merge worktree onto main", persistence.TaskFailureClassMergeFailed},
		{"gate_failed", "gate did not pass: has_approvals == true", persistence.TaskFailureClassGateFailed},
		{"invalid_output_loop_beats_invalid_output", "shape retry loop cap hit — schema violation", persistence.TaskFailureClassInvalidOutputLoop},
		{"invalid_output", "could not parse result.json — invalid json", persistence.TaskFailureClassInvalidOutput},
		{"workflow_role", "role researcher not present in swarm", persistence.TaskFailureClassWorkflowRole},
		{"workflow_cfg", "workflow not found: trading", persistence.TaskFailureClassWorkflowCfg},
		{"llm_5xx", "chat provider returned 503 service unavailable", persistence.TaskFailureClassLLMError},
		{"rate_limited", "too many requests; please retry", persistence.TaskFailureClassRateLimited},
		{"budget", "over budget — daily spend cap exceeded", persistence.TaskFailureClassBudgetBlocked},
		{"lease_expired", "task lease expired during execution", persistence.TaskFailureClassLeaseExpired},
		{"orphaned", "orphaned execution detected; container missing", persistence.TaskFailureClassOrphaned},
		{"tool_generic", "tool runtime: podman exited", persistence.TaskFailureClassToolError},
		{"cancelled_text", "step cancelled by operator", persistence.TaskFailureClassCancelled},
		{"timeout_text", "operation timed out", persistence.TaskFailureClassTimeout},
		{"unknown", "the agent decided to give up", persistence.TaskFailureClassUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyExecutionFailure(nil, c.hint)
			assert.Equal(t, c.expect, got, "hint=%q", c.hint)
		})
	}
}

// TestClassifyExecutionFailure_ErrAndHintMerge confirms the merge
// rule: both err.Error() and hint contribute to the matching
// corpus, so a class signal in either half is enough. Practically
// this is what lets the runtime pass `(wrappedErr, "")` AND
// `(nil, freeText)` without a special path for each shape.
func TestClassifyExecutionFailure_ErrAndHintMerge(t *testing.T) {
	err := errors.New("rate limit") // signal lives in err
	assert.Equal(t, persistence.TaskFailureClassRateLimited,
		ClassifyExecutionFailure(err, ""))
	// Signal in hint, err is unrelated.
	assert.Equal(t, persistence.TaskFailureClassRateLimited,
		ClassifyExecutionFailure(errors.New("oh no"), "rate limit"))
	// Signal split across both: only the merged corpus contains
	// "secret_leak" so the merge must happen.
	err2 := fmt.Errorf("blocked: %w", errors.New("secret_leak: 1"))
	assert.Equal(t, persistence.TaskFailureClassSecretLeak,
		ClassifyExecutionFailure(err2, ""))
}

// TestClassifyExecutionFailure_CancelWrappedByTerminalError — restart-induced
// in-flight FAILED, 2026-06-21. Fix-3 changes resolveTerminalOutcome to wrap
// the underlying cause with %w so the cancel chain survives to the classifier.
// Before Fix-3, fmt.Errorf("%s", detail) severed errors.Is(err,
// context.Canceled), making ClassifyExecutionFailure fall through to UNKNOWN.
// After Fix-3, the wrapped error carries the cancel cause and must classify as
// CANCELLED (the same class as a direct context.Canceled).
func TestClassifyExecutionFailure_CancelWrappedByTerminalError(t *testing.T) {
	// Simulate the shape resolveTerminalOutcome produces after Fix-3:
	// the detail message wraps the underlying context.Canceled with %w.
	cause := context.Canceled
	wrapped := fmt.Errorf("Adaptive routing failed (last step: route): %w", cause)
	got := ClassifyExecutionFailure(wrapped, "")
	assert.Equal(t, persistence.TaskFailureClassCancelled, got,
		"context.Canceled wrapped by a terminal detail message must classify as CANCELLED, not UNKNOWN")
}

// TestContainsAny_DirectEdgesHelper exercises the small helper that
// pattern-matches the lowered-text corpus. Coverage on a trivial
// helper is cheap insurance — a refactor that changes its semantics
// would silently break every classifier branch.
func TestContainsAny_DirectEdgesHelper(t *testing.T) {
	assert.True(t, containsAny("hello world", "world"))
	assert.True(t, containsAny("hello world", "missing", "world"))
	assert.False(t, containsAny("hello world", "missing", "absent"))
	assert.False(t, containsAny("hello", "WORLD"), "containsAny is case-sensitive; caller must lower first")
}
