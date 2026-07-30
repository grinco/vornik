package scheduler

import (
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// classifySchedulerFailure decides what to stamp into last_error_class when the scheduler
// releases a failed task.
//
// INCIDENT 2026-07-30, customer deployment. A task exhausted three attempts and finished
// with `last_error = "task left executor in non-terminal status RUNNING"` and
// **last_error_class EMPTY**. The watchdog had classified the EXECUTION row as
// STUCK_EXECUTION nine seconds later, but the task — the object an operator inspects, and
// the one `vornikctl task explain` reads — carried nothing. So `task explain` had no class
// to explain and `playbook show` had no class to look up, and the playbook corpus the
// troubleshooting guide calls "the highest-value and least-known path in the product" was
// unreachable from the failure it was written for.
//
// Cause: Scheduler.TaskCompleted built ReleaseOptions with Error and never ErrorClass.
// ReleaseOptions.ErrorClass has existed all along; its own comment says an empty value
// "preserves the existing column when a caller hasn't (yet) been updated to classify its
// failures — enables progressive rollout". The scheduler was one of the never-updated
// callers.
//
// Returning "" is meaningful: it PRESERVES whatever is already on the row. That matters
// because the executor stamps precise classes (TOOL_ITERATION_LIMIT, SECRET_LEAK,
// WORKFLOW_DRIFT) before the scheduler's release runs, and overwriting one of those with a
// generic value would destroy information — making the report worse than the empty column
// this change exists to fix.
func classifySchedulerFailure(task *persistence.Task, errorMsg string) string {
	// A precise class already on the row wins. UNKNOWN is not precise, so it does not
	// block a later attempt at classification.
	if task != nil && task.LastErrorClass != nil {
		existing := strings.TrimSpace(*task.LastErrorClass)
		if existing != "" && existing != persistence.TaskFailureClassUnknown {
			return ""
		}
	}

	if cls := classifyByMessage(errorMsg); cls != "" {
		return cls
	}

	// Nothing recognised. UNKNOWN is the documented fallback — the corpus describes it as
	// "the classifier didn't match the failure to any known pattern; most often means a new
	// failure mode" — and it has a playbook entry, so the operator lands somewhere real
	// rather than on an empty column.
	return persistence.TaskFailureClassUnknown
}

// classifyByMessage maps the scheduler's own failure strings to classes.
//
// Deliberately narrow: only shapes the scheduler itself produces, matched on substrings it
// owns. It does not try to classify arbitrary upstream errors — that is the executor's job,
// and guessing here would produce confidently wrong classes, which is worse for an operator
// than UNKNOWN.
//
// NOTE for whoever extends this: "task left executor in non-terminal status" arguably
// deserves its own class and playbook entry. It is left as UNKNOWN on purpose rather than
// mapped to STUCK_EXECUTION, which the model docs reserve for what the WATCHDOG assigns
// when an execution stops advancing. Borrowing it here would blur the taxonomy, and
// inventing a new class without also writing its playbook entry would just relocate the
// dead end this fix removes.
func classifyByMessage(errorMsg string) string {
	msg := strings.ToLower(errorMsg)
	switch {
	case msg == "":
		return ""
	case strings.Contains(msg, "lease") && strings.Contains(msg, "expired"):
		return persistence.TaskFailureClassLeaseExpired
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "timed out"):
		return persistence.TaskFailureClassTimeout
	case strings.Contains(msg, "budget"):
		return persistence.TaskFailureClassBudgetBlocked
	case strings.Contains(msg, "cancel"):
		return persistence.TaskFailureClassCancelled
	}
	return ""
}
