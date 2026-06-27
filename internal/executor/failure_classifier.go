package executor

import (
	"context"
	"errors"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// ClassifyExecutionFailure maps a Go error surfaced from execution to a
// typed failure class recorded alongside the freeform message on the
// task row. Purpose: let operators group failures ("how many LLM 5xx in
// the last day?", "which projects are hitting gate failures?") without
// string-matching the log lines. The mapping is deliberately narrow —
// ambiguous errors fall to UNKNOWN rather than being guessed, because
// a wrong classification is worse than no classification when it drives
// retry policy.
//
// Call this at the executor's terminal-failure points and pass the
// returned class on ReleaseOptions.ErrorClass.
func ClassifyExecutionFailure(err error, hint string) string {
	if err == nil && hint == "" {
		return ""
	}
	var text string
	if err != nil {
		text = err.Error()
	}
	if hint != "" {
		// Hint + message increases signal density; merge with a newline
		// because neither half should contain one and the merged body
		// is only pattern-matched below.
		text = hint + "\n" + text
	}
	lowered := strings.ToLower(text)

	// Typed classes first — an error that knows its own class is
	// authoritative and immune to message-text drift. The N4 delegation
	// guards raise *delegationGuardError, which implements this interface.
	var classed interface{ FailureClass() string }
	if errors.As(err, &classed) {
		if c := classed.FailureClass(); c != "" {
			return c
		}
	}

	// Structural classes first — context cancellation / deadline are
	// the most reliable signals because they come from stdlib errors
	// rather than string munging.
	if errors.Is(err, context.DeadlineExceeded) {
		return persistence.TaskFailureClassTimeout
	}
	if errors.Is(err, context.Canceled) {
		return persistence.TaskFailureClassCancelled
	}

	switch {
	// Hallucinated-placement is matched FIRST because its surrounding
	// verifier wrapper ("phase-2 verifier(s) failed: ...") also
	// contains the substring "tool" (the verifier mentions the broker
	// tool name in its detail), and we don't want the generic "tool"
	// fall-through to mask the more specific class. The needle is the
	// verifier detail's stable lead phrase, written deliberately in
	// the verifier so the classifier can lift it back out without
	// fragile string-matching against the audit-vs-claim counts.
	case containsAny(lowered, "hallucinated placement"):
		return persistence.TaskFailureClassHallucinatedPlacement
	// Secret-leak block is matched next because the message body is
	// authoritative — the executor sets it to "secret_leak: N
	// finding(s)" via ErrSecretLeakBlocked. We don't want a later
	// branch (e.g. the generic "tool" fall-through) to mask it.
	case containsAny(lowered, "secret_leak", "secret leak"):
		return persistence.TaskFailureClassSecretLeak
	// Tool-iteration cap is matched BEFORE the generic "tool" branch
	// because the agent's hardcoded message contains "Tool iteration
	// limit" — without this earlier match, every iteration-cap
	// failure would land in TOOL_ERROR and the checkpoint+continue
	// machinery would never see it.
	case containsAny(lowered, "tool iteration limit", "tool iteration cap"):
		return persistence.TaskFailureClassToolIterationLimit
	case containsAny(lowered, "merge", "worktree") && containsAny(lowered, "fail", "could not"):
		return persistence.TaskFailureClassMergeFailed
	case containsAny(lowered, "gate ", "gate_failed", "condition", "gate did not pass"):
		return persistence.TaskFailureClassGateFailed
	// Loop-cap escalation must be matched BEFORE the generic
	// schema-violation branch — its message contains "schema
	// violation" too (it's an escalation of one), but the typed
	// class is INVALID_OUTPUT_LOOP, not the per-attempt
	// INVALID_OUTPUT, so the dashboard can distinguish "one bad
	// attempt" from "stuck in a loop the watchdog killed".
	case containsAny(lowered, "shape retry loop cap hit"):
		return persistence.TaskFailureClassInvalidOutputLoop
	case containsAny(lowered, "invalid json", "parse", "unmarshal", "schema violation", "could not parse", "plausibility violation"):
		return persistence.TaskFailureClassInvalidOutput
	case containsAny(lowered, "role ", " researcher", "not present in swarm", "workflow role"):
		return persistence.TaskFailureClassWorkflowRole
	case containsAny(lowered, "workflow not found", "step .* not found", "workflow configuration"):
		return persistence.TaskFailureClassWorkflowCfg
	case containsAny(lowered, "chat provider", "llm", "anthropic", "openai", "gateway", "upstream", "502 bad gateway", "503 service", "stream closed"):
		return persistence.TaskFailureClassLLMError
	case containsAny(lowered, "rate limit", "too many requests", "429"):
		return persistence.TaskFailureClassRateLimited
	case containsAny(lowered, "budget", "spend cap", "over budget"):
		return persistence.TaskFailureClassBudgetBlocked
	case containsAny(lowered, "lease expired", "lease recovery"):
		return persistence.TaskFailureClassLeaseExpired
	case containsAny(lowered, "orphaned execution"):
		return persistence.TaskFailureClassOrphaned
	case containsAny(lowered, "tool ", "shell", "exec", "podman", "container", "runtime"):
		return persistence.TaskFailureClassToolError
	case containsAny(lowered, "cancelled", "canceled"):
		return persistence.TaskFailureClassCancelled
	case containsAny(lowered, "timeout", "timed out", "deadline"):
		return persistence.TaskFailureClassTimeout
	}

	return persistence.TaskFailureClassUnknown
}

// containsAny returns true when any of the needles appears in hay. The
// needles are lowercased literals; hay is expected to already be
// lowercased by the caller.
func containsAny(hay string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}
