package executor

import (
	"time"

	"vornik.io/vornik/internal/registry"
)

// resolvedRetry is a step's retry ladder settings with every default already
// applied, so the loop reads one struct instead of branching on absent config.
type resolvedRetry struct {
	// MaxAttempts is the loop bound. Always positive.
	MaxAttempts int
	// BaseDelay is the first sleep. Always positive — a zero here would turn
	// the ladder into a hot loop.
	BaseDelay time.Duration
	// On is the set of step error classes this step retries IN ADDITION to
	// whatever isInfraFailure already recognises. Never subtractive.
	On map[string]bool
}

// resolveStepRetry applies a step's `retry:` block over the built-in defaults.
//
// Every unset or nonsensical field falls back to the constant, so a step with
// no block — or a block with a typo'd duration — behaves exactly as it did
// before step retry was configurable. That is the design's G3, and it is what
// makes this change safe to ship to ten workflows at once.
//
// Nonsense is clamped rather than honoured: a zero or negative max_attempts
// would disable retry entirely and an unparseable delay would resolve to zero,
// and neither is a thing an operator can have meant. Load-time validation
// rejects them too; this is the second line.
func resolveStepRetry(step registry.WorkflowStep) resolvedRetry {
	out := resolvedRetry{
		MaxAttempts: infraRetryMaxAttempts,
		BaseDelay:   infraRetryBaseDelay,
	}
	if step.Retry.MaxAttempts > 0 {
		out.MaxAttempts = step.Retry.MaxAttempts
	}
	if d, err := time.ParseDuration(step.Retry.InitialDelay); err == nil && d > 0 {
		out.BaseDelay = d
	}
	if len(step.Retry.On) > 0 {
		out.On = make(map[string]bool, len(step.Retry.On))
		for _, c := range step.Retry.On {
			out.On[c] = true
		}
	}
	return out
}

// shouldRetry reports whether this step's ladder should re-run after err.
//
// The built-in predicate is checked FIRST and is sufficient on its own: a
// configured `on:` list can only ever add to it. That ordering is the
// widening guarantee expressed in code — there is no path by which a config
// value causes shouldRetry to return false where isInfraFailure returns true.
func (r resolvedRetry) shouldRetry(err error) bool {
	if isInfraFailure(err) {
		return true
	}
	if len(r.On) == 0 {
		return false
	}
	// Classify the same way the step-outcome row does, so what an operator
	// sees in `error_class` is exactly what they write in `on:`. The class
	// strings are internal/stepoutcome's vocabulary, validated against it at
	// load time by Workflow.validateRetryClasses.
	_, class := refineAgentFailureOutcome(errorBeforeLogTail(err.Error()))
	return r.On[class]
}

// retryAttemptLimitReached is the single attempt-budget check used by the
// executor loop. Keeping it on the resolved config prevents the historical
// hard-coded default from silently truncating an explicit max_attempts value.
func retryAttemptLimitReached(attempt int, r resolvedRetry) bool {
	return attempt >= r.MaxAttempts
}
