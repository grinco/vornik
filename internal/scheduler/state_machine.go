package scheduler

import (
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// TransitionTrigger names the cause of a state change. Validates
// that operator-driven transitions only originate from operator
// actions (ResumeOperator can't fire on a RUNNING task; only the
// executor's terminal-success path can land COMPLETED).
//
// See https://docs.vornik.io
// §3.1 (state machine) and §7 (re-execution triggers).
type TransitionTrigger string

const (
	// Daemon-initiated transitions (scheduler / executor).
	TriggerEnqueue         TransitionTrigger = "enqueue"          // → QUEUED
	TriggerLease           TransitionTrigger = "lease"            // → LEASED
	TriggerStart           TransitionTrigger = "start"            // LEASED → RUNNING
	TriggerExecutionDone   TransitionTrigger = "execution_done"   // RUNNING → COMPLETED
	TriggerExecutionFailed TransitionTrigger = "execution_failed" // RUNNING → FAILED
	TriggerCheckpointEmit  TransitionTrigger = "checkpoint_emit"  // RUNNING → AWAITING_INPUT
	TriggerExternalWait    TransitionTrigger = "external_wait"    // RUNNING → AWAITING_EXTERNAL
	TriggerLeaseExpired    TransitionTrigger = "lease_expired"    // LEASED/RUNNING → QUEUED (recovery)
	TriggerWaitChildren    TransitionTrigger = "wait_children"    // RUNNING → WAITING_FOR_CHILDREN
	TriggerChildrenDone    TransitionTrigger = "children_done"    // WAITING_FOR_CHILDREN → COMPLETED|FAILED

	// Operator-initiated transitions.
	TriggerOperatorAnswer    TransitionTrigger = "operator_answer"    // AWAITING_INPUT → QUEUED
	TriggerOperatorDirective TransitionTrigger = "operator_directive" // any non-terminal → QUEUED
	TriggerOperatorAmend     TransitionTrigger = "operator_amend"     // any non-terminal → QUEUED
	TriggerOperatorPause     TransitionTrigger = "operator_pause"     // active → PAUSED
	TriggerOperatorResume    TransitionTrigger = "operator_resume"    // PAUSED → QUEUED
	TriggerOperatorCancel    TransitionTrigger = "operator_cancel"    // any non-terminal → CANCELLED
	TriggerOperatorClose     TransitionTrigger = "operator_close"     // COMPLETED / FAILED / AWAITING_* → CLOSED
	TriggerOperatorApprove   TransitionTrigger = "operator_approve"   // AWAITING_APPROVAL → QUEUED
	TriggerOperatorReject    TransitionTrigger = "operator_reject"    // AWAITING_APPROVAL → CANCELLED

	// External signals.
	TriggerExternalEvent    TransitionTrigger = "external_event"    // AWAITING_EXTERNAL → QUEUED
	TriggerExternalDeadline TransitionTrigger = "external_deadline" // AWAITING_EXTERNAL → QUEUED
)

// IsTerminalTaskStatus reports whether a status is end-of-life
// under the conversational lifecycle. COMPLETED is intentionally
// NOT terminal — it means "this execution finished cleanly; the
// task is awaiting operator input or operator closure". Only
// FAILED, CANCELLED, and CLOSED are absorbing states.
//
// Pre-Phase-23 callers used a private helper that also treated
// COMPLETED as terminal. That helper still exists in scheduler.go
// for the narrower "should I re-lease this task" question; this
// one is the public contract for the conversational lifecycle.
func IsTerminalTaskStatus(status persistence.TaskStatus) bool {
	switch status {
	case persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed:
		return true
	default:
		return false
	}
}

// IsActiveTaskStatus reports whether a task is in an executor-
// reachable state (anything that could lead to a RUNNING execution
// without operator action). Distinct from "non-terminal" because
// AWAITING_INPUT / PAUSED are non-terminal but inactive (need an
// operator signal).
func IsActiveTaskStatus(status persistence.TaskStatus) bool {
	switch status {
	case persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren:
		return true
	default:
		return false
	}
}

// IsAwaitingInput reports whether the task is blocked on a
// human-or-event signal. Powers the inbox query + UI badges. Despite
// the name it covers every "awaiting a human/event action" status:
// AWAITING_INPUT (checkpoint answer), AWAITING_EXTERNAL (event/deadline),
// and AWAITING_APPROVAL (autonomy manual-approval gate). The name is
// kept for blast-radius reasons — many callers key UI affordances off it.
func IsAwaitingInput(status persistence.TaskStatus) bool {
	return status == persistence.TaskStatusAwaitingInput ||
		status == persistence.TaskStatusAwaitingExternal ||
		status == persistence.TaskStatusAwaitingApproval
}

// ValidateTransition reports whether `from → to` is a permitted
// state change under the trigger.
//
// Pure function — no DB / no time. The caller (scheduler /
// executor / API handler) checks the verdict before mutating the
// task row. A nil error means the change is allowed; a non-nil
// error carries a human-readable reason suitable for HTTP 409
// responses.
//
// Design invariants enforced:
//
//   - CLOSED, FAILED, CANCELLED are absorbing — no transition out.
//     (FAILED → QUEUED happens via the scheduler's retry path,
//     which uses TriggerEnqueue and is tested separately; this
//     function refuses operator-driven re-opens of FAILED tasks.)
//   - Operator triggers can never land in a daemon-only status
//     (e.g. an operator can't issue TriggerStart).
//   - Daemon triggers (start/done/failed) never land in PAUSED or
//     CLOSED — those are operator-only destinations.
//   - The new statuses can only be reached from RUNNING (the
//     executor decides) or from PAUSED (operator resumes), never
//     from QUEUED directly.
func ValidateTransition(from, to persistence.TaskStatus, trigger TransitionTrigger) error {
	if from == to {
		// Idempotent transitions are allowed for triggers that may
		// fire concurrently (e.g. operator_resume from PAUSED → PAUSED
		// races; the second caller is a no-op). Specific triggers
		// that should never be self-loops are filtered below.
		switch trigger {
		case TriggerOperatorPause, TriggerOperatorResume,
			TriggerOperatorClose, TriggerOperatorCancel:
			return nil
		}
		return fmt.Errorf("invalid transition: %s → %s under trigger %s (no-op self-loop)", from, to, trigger)
	}

	// Absorbing states — no transition out except via the scheduler's
	// own retry path (which uses raw inserts, not this validator).
	// One deliberate exception: an operator may close a FAILED task
	// ("Close — won't pursue"), a terminal→terminal acknowledgement
	// that retires a dead-lettered task. CANCELLED and CLOSED stay
	// fully absorbing. The OperatorClose case below validates the rest.
	if IsTerminalTaskStatus(from) {
		closingFailed := trigger == TriggerOperatorClose &&
			from == persistence.TaskStatusFailed &&
			to == persistence.TaskStatusClosed
		if !closingFailed {
			return fmt.Errorf("invalid transition: %s is terminal; cannot transition to %s", from, to)
		}
	}

	// Trigger × (from → to) compatibility table. Default-deny: any
	// case not listed here is rejected.
	switch trigger {

	// ---- Daemon-initiated ----
	case TriggerEnqueue:
		// PENDING → QUEUED, or any non-terminal recovery path
		// (FAILED in retry land would call this too, but FAILED is
		// terminal here — retries skip this function).
		if to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected → QUEUED")
		}
		// Allowed origins: PENDING, AWAITING_INPUT (operator answer
		// → re-queue), AWAITING_EXTERNAL (event/deadline), PAUSED
		// (operator resume), COMPLETED (operator amend/directive),
		// LEASED/RUNNING (recovery, lease-expired path).
		switch from {
		case persistence.TaskStatusPending,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
			persistence.TaskStatusPaused,
			persistence.TaskStatusCompleted,
			persistence.TaskStatusLeased,
			persistence.TaskStatusRunning:
			return nil
		}
		return triggerErr(trigger, from, to, "origin not enqueueable")

	case TriggerLease:
		if from != persistence.TaskStatusQueued || to != persistence.TaskStatusLeased {
			return triggerErr(trigger, from, to, "expected QUEUED → LEASED")
		}
		return nil

	case TriggerStart:
		if from != persistence.TaskStatusLeased || to != persistence.TaskStatusRunning {
			return triggerErr(trigger, from, to, "expected LEASED → RUNNING")
		}
		return nil

	case TriggerExecutionDone:
		if from != persistence.TaskStatusRunning || to != persistence.TaskStatusCompleted {
			return triggerErr(trigger, from, to, "expected RUNNING → COMPLETED")
		}
		return nil

	case TriggerExecutionFailed:
		if from != persistence.TaskStatusRunning || to != persistence.TaskStatusFailed {
			return triggerErr(trigger, from, to, "expected RUNNING → FAILED")
		}
		return nil

	case TriggerCheckpointEmit:
		if from != persistence.TaskStatusRunning || to != persistence.TaskStatusAwaitingInput {
			return triggerErr(trigger, from, to, "expected RUNNING → AWAITING_INPUT")
		}
		return nil

	case TriggerExternalWait:
		if from != persistence.TaskStatusRunning || to != persistence.TaskStatusAwaitingExternal {
			return triggerErr(trigger, from, to, "expected RUNNING → AWAITING_EXTERNAL")
		}
		return nil

	case TriggerLeaseExpired:
		if to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected → QUEUED")
		}
		if from != persistence.TaskStatusLeased && from != persistence.TaskStatusRunning {
			return triggerErr(trigger, from, to, "lease can expire only from LEASED/RUNNING")
		}
		return nil

	case TriggerWaitChildren:
		if from != persistence.TaskStatusRunning || to != persistence.TaskStatusWaitingForChildren {
			return triggerErr(trigger, from, to, "expected RUNNING → WAITING_FOR_CHILDREN")
		}
		return nil

	case TriggerChildrenDone:
		if from != persistence.TaskStatusWaitingForChildren {
			return triggerErr(trigger, from, to, "expected from WAITING_FOR_CHILDREN")
		}
		if to != persistence.TaskStatusCompleted && to != persistence.TaskStatusFailed {
			return triggerErr(trigger, from, to, "must land COMPLETED or FAILED")
		}
		return nil

	// ---- Operator-initiated ----
	case TriggerOperatorAnswer:
		if from != persistence.TaskStatusAwaitingInput || to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected AWAITING_INPUT → QUEUED")
		}
		return nil

	case TriggerOperatorDirective, TriggerOperatorAmend:
		if to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected → QUEUED")
		}
		// LLD §7 trigger 2: directive during RUNNING is queued for
		// next round; we still allow the trigger to fire (writes the
		// message + bumps updated_at) but the status doesn't change.
		// That branch is the caller's job; this validator allows
		// any non-terminal origin.
		if IsTerminalTaskStatus(from) {
			return triggerErr(trigger, from, to, "task is terminal")
		}
		// Directive while RUNNING is policy "queue for next round" —
		// the scheduler keeps RUNNING, the directive lands in the
		// thread, and the next execution sees it. This validator
		// is asked specifically about RUNNING → QUEUED, which is
		// not what happens in that case; reject it so callers must
		// check IsActiveTaskStatus(RUNNING) themselves before
		// invoking ValidateTransition.
		if from == persistence.TaskStatusRunning || from == persistence.TaskStatusLeased {
			return triggerErr(trigger, from, to, "directive during running execution must queue, not transition")
		}
		return nil

	case TriggerOperatorPause:
		if to != persistence.TaskStatusPaused {
			return triggerErr(trigger, from, to, "expected → PAUSED")
		}
		if !IsActiveTaskStatus(from) && !IsAwaitingInput(from) {
			return triggerErr(trigger, from, to, "can only pause an active or awaiting task")
		}
		return nil

	case TriggerOperatorResume:
		if from != persistence.TaskStatusPaused || to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected PAUSED → QUEUED")
		}
		return nil

	case TriggerOperatorCancel:
		if to != persistence.TaskStatusCancelled {
			return triggerErr(trigger, from, to, "expected → CANCELLED")
		}
		if IsTerminalTaskStatus(from) {
			return triggerErr(trigger, from, to, "task already terminal")
		}
		return nil

	case TriggerOperatorClose:
		if to != persistence.TaskStatusClosed {
			return triggerErr(trigger, from, to, "expected → CLOSED")
		}
		// Close-eligible from:
		//   COMPLETED       — normal "ack & archive".
		//   AWAITING_INPUT  — operator dismissed the open checkpoint
		//                     and called it done.
		//   AWAITING_EXTERNAL — operator declared the wait satisfied.
		//   FAILED          — operator gives up after retries are
		//                     exhausted ("Close — won't pursue"). The
		//                     UI offers this on every failed task
		//                     (internal/ui/recovery_actions.go), so the
		//                     state machine must permit it or the button
		//                     is a silent no-op.
		switch from {
		case persistence.TaskStatusCompleted,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
			persistence.TaskStatusFailed:
			return nil
		}
		return triggerErr(trigger, from, to, "task must be COMPLETED, FAILED, or AWAITING_* to close")

	case TriggerOperatorApprove:
		// Autonomy manual-approval gate: the operator approved a task
		// parked in AWAITING_APPROVAL, so it joins the normal queue.
		// Only AWAITING_APPROVAL is approvable — a stuck PENDING task
		// is NOT (it has no pending approval; the UI retry path handles
		// that case separately).
		if from != persistence.TaskStatusAwaitingApproval || to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected AWAITING_APPROVAL → QUEUED")
		}
		return nil

	case TriggerOperatorReject:
		// Operator declined an approval-gated task — it never runs.
		// CANCELLED is the absorbing "operator said no" destination.
		if from != persistence.TaskStatusAwaitingApproval || to != persistence.TaskStatusCancelled {
			return triggerErr(trigger, from, to, "expected AWAITING_APPROVAL → CANCELLED")
		}
		return nil

	// ---- External signals ----
	case TriggerExternalEvent, TriggerExternalDeadline:
		if from != persistence.TaskStatusAwaitingExternal || to != persistence.TaskStatusQueued {
			return triggerErr(trigger, from, to, "expected AWAITING_EXTERNAL → QUEUED")
		}
		return nil
	}

	return fmt.Errorf("unknown trigger %q", trigger)
}

func triggerErr(trigger TransitionTrigger, from, to persistence.TaskStatus, why string) error {
	return fmt.Errorf("invalid transition: %s → %s under trigger %s (%s)", from, to, trigger, why)
}
