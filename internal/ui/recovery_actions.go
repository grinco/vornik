package ui

import (
	"vornik.io/vornik/internal/persistence"
)

// RecoveryAction is one suggested next-step the operator can take
// from a FAILED task's detail page. Designed for the task_detail
// "Recovery actions" card (Upgrade #1 in
// https://docs.vornik.io § 2026-05-26).
//
// Each action is either a one-click POST or a navigation to a page
// where the relevant action surface (hint compose, fork modal) is
// already pre-filled. We deliberately do NOT inline a modal-trigger
// JS shape — every action is a plain link or form, so screen readers
// + no-JS clients get an identical surface.
type RecoveryAction struct {
	// Label is the button text. Imperative, short ("Retry", "Fork
	// from step", "Close & explain"). Cap ~30 chars.
	Label string
	// URL is the destination — relative path under /ui/ or an
	// absolute URL. For POST actions this is the form action; for
	// GET it's the href.
	URL string
	// Method is "GET" (link) or "POST" (form). Anything else is
	// rejected by the template render.
	Method string
	// Variant controls visual treatment:
	//   "primary"   — filled accent button; one per class
	//   "secondary" — outline button; the default for safe actions
	//   "danger"    — rose-tinted; reserved for "close" / "abort"
	Variant string
	// Tooltip is a one-line hover description. Optional.
	Tooltip string
	// Confirm, when non-empty, fires a window.confirm() prompt
	// before submission. Use sparingly — operators on phone-from-
	// couch hate extra taps.
	Confirm string
}

// SteerPrefillFor returns a failure-derived starter hint for the
// "Compose a steering hint" textarea on a FAILED task's recovery
// card. The recovery card's "Steer + retry" action opens that
// compose box, but before this it landed empty — operators had to
// recall the failure context themselves (2026-05-29 LLD-drift audit
// §8.6, low). The prefill seeds a class-appropriate template the
// operator edits before sending; it is NOT auto-submitted.
//
// Returns "" for classes where a generic prefill would be noise
// (the operator types from scratch). The JS only seeds the textarea
// when it's empty, so a returning operator's draft is never clobbered.
func SteerPrefillFor(class string) string {
	switch class {
	case persistence.TaskFailureClassToolIterationLimit:
		return "The previous attempt burned its tool/iteration budget without converging. Narrow the scope so the next run finishes: "
	case persistence.TaskFailureClassInvalidOutput,
		persistence.TaskFailureClassInvalidOutputLoop:
		return "The previous attempt produced output that failed schema validation. Correct it explicitly — name the field that was wrong and the shape it must take: "
	case persistence.TaskFailureClassRateLimited:
		return "The previous attempt hit rate limits / transient 429s. If a specific source is the problem, tell the next run to skip or substitute it: "
	default:
		return ""
	}
}

// RecoveryActionsFor returns the per-class action set for a FAILED
// task. The mapping is data-driven so future failure classes are a
// single-case addition. Tasks without a failure class (or in a
// non-failed status) get an empty list — the caller skips rendering.
//
// Design notes:
//   - One PRIMARY action per class. It's the recommended path.
//   - Universal fallbacks (Retry as-is, Close & explain) are added
//     at the END so each class can specialise the prefix without
//     duplicating the suffix.
//   - "Pre-fill via query param" pattern (e.g. ?steer=open) lets the
//     target page open the relevant compose / modal already focused.
//     The target pages read these params at render time; no new
//     endpoint needed.
//
// taskID is required for endpoint URL construction. Empty taskID
// returns nil — defensive against rendering bugs that lose the ID.
func RecoveryActionsFor(class string, taskID string) []RecoveryAction {
	if taskID == "" {
		return nil
	}

	// Per-class prefix — the recommended PRIMARY action plus any
	// class-specific secondary actions that come before the
	// universal fallbacks.
	var actions []RecoveryAction

	switch class {
	case persistence.TaskFailureClassRateLimited:
		// The 2026-05-26 scraper backoff retries transient 429/503
		// in-process now, so a plain requeue has materially better
		// odds than it used to. Primary = requeue. Secondary = open
		// in live view so the operator can steer (e.g. inject "skip
		// portal X this cycle"). Verb renamed 2026-05-26 refactor B
		// for consistency with the new "Rerun from step" surface
		// on execution detail.
		actions = append(actions,
			RecoveryAction{
				Label:   "Requeue — backoff layer will absorb transients",
				URL:     "/ui/tasks/" + taskID + "/retry",
				Method:  "POST",
				Variant: "primary",
				Tooltip: "Scraper now retries 429/503 with exponential backoff in-process. A fresh attempt has a real shot.",
				Confirm: "Requeue this task? Resets the attempt counter.",
			},
		)

	case persistence.TaskFailureClassToolIterationLimit:
		// Tool budget burned without converging. Retry alone usually
		// fails the same way — but adding a steering hint that
		// scopes the request lets the agent converge.
		actions = append(actions,
			RecoveryAction{
				Label:   "Steer + retry",
				URL:     "/ui/tasks/" + taskID + "?steer=open",
				Method:  "GET",
				Variant: "primary",
				Tooltip: "Add a steering hint (e.g. \"focus only on portal X\") then retry — bare retry usually repeats the iteration burn.",
			},
		)

	case persistence.TaskFailureClassInvalidOutput,
		persistence.TaskFailureClassInvalidOutputLoop:
		// Schema-shape failures benefit from a corrective hint that
		// names which key was wrong. The hint UI lets the operator
		// add that context before retry.
		actions = append(actions,
			RecoveryAction{
				Label:   "Steer + retry",
				URL:     "/ui/tasks/" + taskID + "?steer=open",
				Method:  "GET",
				Variant: "primary",
				Tooltip: "Add a corrective hint (e.g. \"produced_files must list the actual paths you wrote\") then retry.",
			},
		)

	case persistence.TaskFailureClassWorkflowRole,
		persistence.TaskFailureClassWorkflowCfg,
		persistence.TaskFailureClassWorkflowDrift:
		// Configuration issues — retrying the same task with the
		// same broken workflow won't help. Send the operator to
		// the workflow editor first.
		actions = append(actions,
			RecoveryAction{
				Label:   "Review workflow config",
				URL:     "/ui/workflows",
				Method:  "GET",
				Variant: "primary",
				Tooltip: "The workflow definition couldn't be loaded. Fix the YAML, then retry.",
			},
		)

	case persistence.TaskFailureClassBudgetBlocked:
		// Project budget cap tripped. Retry won't help without
		// either a budget bump or a model downgrade.
		actions = append(actions,
			RecoveryAction{
				Label:   "Review project budget",
				URL:     "/ui/spend",
				Method:  "GET",
				Variant: "primary",
				Tooltip: "Project hit its budget cap. Bump the cap, downgrade the role's model, or defer.",
			},
		)

	case persistence.TaskFailureClassChildFailed:
		// A delegated child failed. The parent's retry only makes
		// sense after the child is investigated.
		actions = append(actions,
			RecoveryAction{
				Label:   "Inspect failed child",
				URL:     "/ui/tasks/" + taskID,
				Method:  "GET",
				Variant: "primary",
				Tooltip: "A child task failed. Find which one in the Subtasks panel below and fix root cause before retrying.",
			},
		)

	case persistence.TaskFailureClassSecretLeak,
		persistence.TaskFailureClassHallucinatedPlacement:
		// Safety-class failures — DO NOT auto-retry. The operator
		// must inspect the audit before any reattempt.
		actions = append(actions,
			RecoveryAction{
				Label:   "Inspect audit trail",
				URL:     "/ui/audit",
				Method:  "GET",
				Variant: "primary",
				Tooltip: "Safety class — review the audit log before retrying.",
			},
		)

	case persistence.TaskFailureClassStuckExecution:
		// Watchdog flipped a hung execution. Retry restarts the
		// whole pipeline — often the right call once the operator
		// confirms the root cause cleared.
		actions = append(actions,
			RecoveryAction{
				Label:   "Restart from start",
				URL:     "/ui/tasks/" + taskID + "/retry",
				Method:  "POST",
				Variant: "primary",
				Tooltip: "Execution hung. A retry starts a fresh execution from step 1.",
				Confirm: "Restart this task from the beginning?",
			},
		)

	default:
		// LLM_ERROR, TIMEOUT, TOOL_ERROR, RUNTIME_ERROR, MERGE_FAILED,
		// GATE_FAILED, ORPHANED, LEASE_EXPIRED, CANCELLED, UNKNOWN
		// — generic transient or operator-context failures. Plain
		// retry is the right primary; the operator can always
		// steer-and-retry via the secondary fallback below.
		actions = append(actions,
			RecoveryAction{
				Label:   "Requeue",
				URL:     "/ui/tasks/" + taskID + "/retry",
				Method:  "POST",
				Variant: "primary",
				Tooltip: "Requeue the task as-is — resets the attempt counter and runs from step 1. If this fails the same way, try \"Steer + retry\" instead.",
				Confirm: "Requeue this task? Resets the attempt counter.",
			},
		)
	}

	// Universal "Steer + retry" — appended for every class that
	// didn't already make it the primary. Gives operators an escape
	// hatch even on classes where bare retry is the recommended
	// primary.
	hasSteer := false
	for _, a := range actions {
		if a.URL == "/ui/tasks/"+taskID+"?steer=open" {
			hasSteer = true
			break
		}
	}
	if !hasSteer {
		actions = append(actions,
			RecoveryAction{
				Label:   "Steer + retry",
				URL:     "/ui/tasks/" + taskID + "?steer=open",
				Method:  "GET",
				Variant: "secondary",
				Tooltip: "Inject a corrective hint before the retry. Useful when bare retry will repeat the same failure.",
			},
		)
	}

	// Universal "Close & won't pursue" — every failed task gets the
	// option to give up with explanation. Danger variant signals
	// finality.
	actions = append(actions,
		RecoveryAction{
			Label:   "Close — won't pursue",
			URL:     "/ui/tasks/" + taskID + "/close",
			Method:  "POST",
			Variant: "danger",
			Tooltip: "Mark this task closed without retrying. Captures the operator's decision in the audit log.",
			Confirm: "Close this task without retrying?",
		},
	)

	return actions
}
