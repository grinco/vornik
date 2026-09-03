package persistence

// TaskShouldRetry is the single answer to "does this task get another
// execution attempt?".
//
// WHY IT EXISTS. Six places decided this independently, and every one of them
// asked only about the attempt budget:
//
//	internal/scheduler/scheduler.go:1042, :1074
//	internal/executor/executor.go:2886
//	internal/executor/workflow.go:3420, :3572 (taskWillRetry), :3962
//
// Six spellings of one rule is how the rule drifts. It already had: the
// executor's taskWillRetry documents itself as a hand-copied mirror of the
// scheduler's predicate kept in sync by hand, and it gates whether a failure
// NOTIFICATION fires, whether the circuit breaker trips, and alerting. Teaching
// only the scheduler about permanent failures would have desynchronised them
// silently — the executor computing "this will retry" (so suppressing the
// notification) for a task the scheduler had just failed terminally, leaving the
// operator with no notification at all for exactly the failures the change was
// meant to surface faster.
//
// WHAT CHANGED. Previously: budget only. Now: a terminal class skips the budget
// entirely; everything else is byte-identical to the old behaviour.
//
// See https://docs.vornik.io
func TaskShouldRetry(attempt, maxAttempts int, class string) bool {
	// A failure that cannot succeed on a re-run does not get to spend an
	// attempt. Checked FIRST and independently of the budget: the budget is not
	// the question when the answer cannot change.
	if IsTerminalFailureClass(class) {
		return false
	}
	// MaxAttempts <= 0 means retries are disabled — this is the only attempt.
	// Preserved from taskWillRetry, where a legacy row with an unset budget
	// would otherwise read as "unlimited".
	if maxAttempts <= 0 {
		return false
	}
	if attempt <= 0 {
		attempt = 1
	}
	return attempt < maxAttempts
}

// terminalFailureClasses is the exact set of TASK failure classes that never
// earn another attempt.
//
// DELIBERATELY TINY, and it is an allow-list rather than a heuristic so that
// growing it is a visible, reviewable act. The bar for adding one:
//
//   - the failure cannot succeed on a re-run of the same task, by construction
//     rather than by observation;
//   - it is decided by a TYPED signal at the source, never by matching text;
//   - it ships with a playbook entry, because a class whose remediation is
//     unwritten just relocates the dead end it was meant to remove.
//
// Note CHILD_FAILED is absent on purpose. The bubble-up path stamps it
// unconditionally, flattening a child's permanent failure, and the parent's own
// re-run may well succeed — inheriting a child's permanence would strand a
// recoverable parent.
var terminalFailureClasses = map[string]bool{
	TaskFailureClassForgeTargetUnavailable: true,
}

// IsTerminalFailureClass reports whether a TASK failure class is one that never
// earns another attempt.
//
// Exact match against the task vocabulary. The step vocabulary is disjoint and
// lowercase (enforced by internal/playbook/vocabulary_test.go), so a step class
// can never accidentally satisfy this.
func IsTerminalFailureClass(class string) bool {
	return terminalFailureClasses[class]
}
