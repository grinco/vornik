// Package stepid is the one vocabulary for the synthetic step ids the executor
// writes when it re-runs a step with different parameters. It is a leaf
// package so the telemetry and CLI side can read the same suffixes the executor
// appends without importing the executor.
//
// The executor persists every retry rung as its own execution_step_outcomes
// row under a suffixed id, so "the step" and "the first attempt" are only
// recoverable by stripping or recognising these suffixes. Two readers had
// their own idea of them before this package existed (the executor's
// producer-role lookup and, by omission, workflow-stats, which counted a rung's
// own `ok` as a first-attempt pass — self-evolving-workflows design, addendum
// 2026-09-03).
package stepid

import "strings"

// Suffixes the executor appends to a base step id. All start with `_` and
// never occur in a base step id, so right-trimming the first match is
// unambiguous.
//
//	_shape_retry     — retry.go's shape-violation retry
//	_model_fallback  — retry.go's model-fallback retry
//	_infra_retry<N>  — retry.go's transient-failure retry
//	_refusal_retry   — plan_step.go's refusal retry
//	_route_retry     — workflow.go's strict-adaptive corrective retry
var exactSuffixes = []string{"_shape_retry", "_model_fallback", "_refusal_retry", "_route_retry"}

const infraRetryPrefix = "_infra_retry"

// StripRetrySuffix returns the base step id for a retry rung's id, and the id
// unchanged when it carries no known suffix. A bare suffix with nothing before
// it is not a retry of anything and is returned unchanged.
func StripRetrySuffix(stepID string) string {
	for _, suffix := range exactSuffixes {
		if strings.HasSuffix(stepID, suffix) && len(stepID) > len(suffix) {
			return strings.TrimSuffix(stepID, suffix)
		}
	}
	// _infra_retry<N> has a variable trailing integer — strip from the prefix.
	if idx := strings.Index(stepID, infraRetryPrefix); idx > 0 {
		return stepID[:idx]
	}
	return stepID
}

// IsRetryAttempt reports whether the id names a retry rung rather than a
// step's first attempt.
func IsRetryAttempt(stepID string) bool {
	return StripRetrySuffix(stepID) != stepID
}
