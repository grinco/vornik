package scheduler

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Acceptance tests for ValidateTransition. Mirrors the state-machine
// table in https://docs.vornik.io
// §3.1. Each happy-path row asserts a permitted transition; the
// negative tests assert default-deny.

func TestStateMachine_HappyPath(t *testing.T) {
	cases := []struct {
		name    string
		from    persistence.TaskStatus
		to      persistence.TaskStatus
		trigger TransitionTrigger
	}{
		// Daemon-initiated lifecycle.
		{"enqueue_pending", persistence.TaskStatusPending, persistence.TaskStatusQueued, TriggerEnqueue},
		{"lease", persistence.TaskStatusQueued, persistence.TaskStatusLeased, TriggerLease},
		{"start", persistence.TaskStatusLeased, persistence.TaskStatusRunning, TriggerStart},
		{"execution_done", persistence.TaskStatusRunning, persistence.TaskStatusCompleted, TriggerExecutionDone},
		{"execution_failed", persistence.TaskStatusRunning, persistence.TaskStatusFailed, TriggerExecutionFailed},
		{"checkpoint_emit", persistence.TaskStatusRunning, persistence.TaskStatusAwaitingInput, TriggerCheckpointEmit},
		{"external_wait", persistence.TaskStatusRunning, persistence.TaskStatusAwaitingExternal, TriggerExternalWait},
		{"wait_children", persistence.TaskStatusRunning, persistence.TaskStatusWaitingForChildren, TriggerWaitChildren},
		{"children_complete", persistence.TaskStatusWaitingForChildren, persistence.TaskStatusCompleted, TriggerChildrenDone},
		{"children_fail", persistence.TaskStatusWaitingForChildren, persistence.TaskStatusFailed, TriggerChildrenDone},

		// Recovery.
		{"lease_expired_from_leased", persistence.TaskStatusLeased, persistence.TaskStatusQueued, TriggerLeaseExpired},
		{"lease_expired_from_running", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerLeaseExpired},

		// Operator triggers — re-queue paths.
		{"operator_answer", persistence.TaskStatusAwaitingInput, persistence.TaskStatusQueued, TriggerOperatorAnswer},
		{"directive_from_completed", persistence.TaskStatusCompleted, persistence.TaskStatusQueued, TriggerOperatorDirective},
		{"directive_from_awaiting_input", persistence.TaskStatusAwaitingInput, persistence.TaskStatusQueued, TriggerOperatorDirective},
		{"directive_from_awaiting_external", persistence.TaskStatusAwaitingExternal, persistence.TaskStatusQueued, TriggerOperatorDirective},
		{"amend_from_completed", persistence.TaskStatusCompleted, persistence.TaskStatusQueued, TriggerOperatorAmend},

		// Pause / resume / close.
		{"pause_running", persistence.TaskStatusRunning, persistence.TaskStatusPaused, TriggerOperatorPause},
		{"pause_awaiting_input", persistence.TaskStatusAwaitingInput, persistence.TaskStatusPaused, TriggerOperatorPause},
		{"resume", persistence.TaskStatusPaused, persistence.TaskStatusQueued, TriggerOperatorResume},
		{"close_completed", persistence.TaskStatusCompleted, persistence.TaskStatusClosed, TriggerOperatorClose},
		{"close_from_awaiting_input", persistence.TaskStatusAwaitingInput, persistence.TaskStatusClosed, TriggerOperatorClose},
		{"close_from_awaiting_external", persistence.TaskStatusAwaitingExternal, persistence.TaskStatusClosed, TriggerOperatorClose},
		// Regression: operator "Close — won't pursue" on a FAILED task.
		// The UI offers this button on every failed task; the state
		// machine must permit FAILED → CLOSED or the button silently
		// no-ops (companion-example task_20260603131232 / _20260602214128).
		{"close_from_failed", persistence.TaskStatusFailed, persistence.TaskStatusClosed, TriggerOperatorClose},

		// Cancel from arbitrary non-terminal.
		{"cancel_from_running", persistence.TaskStatusRunning, persistence.TaskStatusCancelled, TriggerOperatorCancel},
		{"cancel_from_paused", persistence.TaskStatusPaused, persistence.TaskStatusCancelled, TriggerOperatorCancel},
		{"cancel_from_awaiting_input", persistence.TaskStatusAwaitingInput, persistence.TaskStatusCancelled, TriggerOperatorCancel},

		// External signals.
		{"external_event", persistence.TaskStatusAwaitingExternal, persistence.TaskStatusQueued, TriggerExternalEvent},
		{"external_deadline", persistence.TaskStatusAwaitingExternal, persistence.TaskStatusQueued, TriggerExternalDeadline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTransition(tc.from, tc.to, tc.trigger); err != nil {
				t.Errorf("expected %s → %s under %s to be allowed, got %v", tc.from, tc.to, tc.trigger, err)
			}
		})
	}
}

func TestStateMachine_TerminalStatesAreAbsorbing(t *testing.T) {
	terminal := []persistence.TaskStatus{
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed,
	}
	// Try a representative trigger from each absorbing state.
	for _, from := range terminal {
		for _, to := range []persistence.TaskStatus{
			persistence.TaskStatusQueued,
			persistence.TaskStatusRunning,
			persistence.TaskStatusCompleted,
		} {
			err := ValidateTransition(from, to, TriggerEnqueue)
			if err == nil {
				t.Errorf("expected %s → %s to be rejected (terminal source); got nil", from, to)
			}
		}
	}
}

func TestStateMachine_OperatorCannotIssueDaemonTriggers(t *testing.T) {
	// An operator trying to "start" a leased task is malformed —
	// only the executor can do that. The validator rejects on
	// origin/destination mismatch.
	err := ValidateTransition(persistence.TaskStatusLeased, persistence.TaskStatusRunning, TriggerOperatorAnswer)
	if err == nil {
		t.Errorf("operator trigger LEASED → RUNNING should be rejected; got nil")
	}
}

func TestStateMachine_DirectiveDuringRunningIsRejected(t *testing.T) {
	// LLD §7 trigger 2: a directive during a running execution
	// queues for next round; the validator refuses RUNNING → QUEUED
	// under TriggerOperatorDirective so the caller is forced to
	// keep RUNNING and just write the message.
	err := ValidateTransition(persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerOperatorDirective)
	if err == nil {
		t.Errorf("directive RUNNING → QUEUED should be rejected; got nil")
	}
	if !strings.Contains(err.Error(), "queue") {
		t.Errorf("error message should explain queueing policy, got %v", err)
	}
}

func TestStateMachine_CloseRequiresCompletedOrAwaiting(t *testing.T) {
	// Operator can't close from RUNNING — must let the execution
	// settle first.
	err := ValidateTransition(persistence.TaskStatusRunning, persistence.TaskStatusClosed, TriggerOperatorClose)
	if err == nil {
		t.Errorf("close from RUNNING should be rejected; got nil")
	}
	// Can't close from QUEUED either.
	err = ValidateTransition(persistence.TaskStatusQueued, persistence.TaskStatusClosed, TriggerOperatorClose)
	if err == nil {
		t.Errorf("close from QUEUED should be rejected; got nil")
	}
}

func TestStateMachine_NoSelfLoopExceptIdempotentOperators(t *testing.T) {
	// PAUSED → PAUSED under TriggerOperatorPause is idempotent
	// (concurrent pauses race); CLOSED → CLOSED is idempotent.
	if err := ValidateTransition(persistence.TaskStatusPaused, persistence.TaskStatusPaused, TriggerOperatorPause); err != nil {
		t.Errorf("idempotent pause should be allowed, got %v", err)
	}
	// RUNNING → RUNNING under any non-idempotent trigger is rejected.
	if err := ValidateTransition(persistence.TaskStatusRunning, persistence.TaskStatusRunning, TriggerStart); err == nil {
		t.Errorf("RUNNING → RUNNING under TriggerStart should be rejected; got nil")
	}
}

func TestStateMachine_LeaseValidation(t *testing.T) {
	// Lease can only fire QUEUED → LEASED.
	if err := ValidateTransition(persistence.TaskStatusPending, persistence.TaskStatusLeased, TriggerLease); err == nil {
		t.Errorf("PENDING → LEASED should be rejected; got nil")
	}
	// Start can only fire LEASED → RUNNING.
	if err := ValidateTransition(persistence.TaskStatusQueued, persistence.TaskStatusRunning, TriggerStart); err == nil {
		t.Errorf("QUEUED → RUNNING should be rejected; got nil")
	}
}

func TestStateMachine_UnknownTrigger(t *testing.T) {
	err := ValidateTransition(persistence.TaskStatusPending, persistence.TaskStatusQueued, TransitionTrigger("invented"))
	if err == nil {
		t.Errorf("unknown trigger should be rejected; got nil")
	}
	if !strings.Contains(err.Error(), "unknown trigger") {
		t.Errorf("error should mention unknown trigger, got %v", err)
	}
}

func TestStateMachine_HelperPredicates(t *testing.T) {
	// IsTerminalTaskStatus
	if !IsTerminalTaskStatus(persistence.TaskStatusClosed) {
		t.Error("CLOSED must be terminal")
	}
	if IsTerminalTaskStatus(persistence.TaskStatusCompleted) {
		t.Error("COMPLETED must NOT be terminal under conversational lifecycle")
	}
	if IsTerminalTaskStatus(persistence.TaskStatusAwaitingInput) {
		t.Error("AWAITING_INPUT must NOT be terminal")
	}

	// IsActiveTaskStatus
	if !IsActiveTaskStatus(persistence.TaskStatusRunning) {
		t.Error("RUNNING must be active")
	}
	if IsActiveTaskStatus(persistence.TaskStatusAwaitingInput) {
		t.Error("AWAITING_INPUT is not active (needs operator signal)")
	}
	if IsActiveTaskStatus(persistence.TaskStatusPaused) {
		t.Error("PAUSED is not active")
	}

	// IsAwaitingInput
	if !IsAwaitingInput(persistence.TaskStatusAwaitingInput) {
		t.Error("AWAITING_INPUT must report awaiting-input")
	}
	if !IsAwaitingInput(persistence.TaskStatusAwaitingExternal) {
		t.Error("AWAITING_EXTERNAL must report awaiting-input")
	}
	if IsAwaitingInput(persistence.TaskStatusRunning) {
		t.Error("RUNNING is not awaiting input")
	}
}

// TestStateMachine_NegativeBranches walks the trigger-specific
// rejection paths that the happy-path matrix above doesn't reach.
// Each row pins one branch: wrong destination, wrong origin, or
// terminal-source rejection.
func TestStateMachine_NegativeBranches(t *testing.T) {
	cases := []struct {
		name    string
		from    persistence.TaskStatus
		to      persistence.TaskStatus
		trigger TransitionTrigger
		wantSub string
	}{
		// TriggerEnqueue: destination must be QUEUED.
		{"enqueue_wrong_dest", persistence.TaskStatusPending, persistence.TaskStatusRunning, TriggerEnqueue, "QUEUED"},
		// TriggerExecutionDone: wrong destination.
		{"execdone_wrong_dest", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerExecutionDone, "RUNNING → COMPLETED"},
		// TriggerExecutionFailed: wrong origin.
		{"execfail_wrong_origin", persistence.TaskStatusLeased, persistence.TaskStatusFailed, TriggerExecutionFailed, "RUNNING → FAILED"},
		// TriggerCheckpointEmit: wrong destination.
		{"checkpoint_wrong_dest", persistence.TaskStatusRunning, persistence.TaskStatusCompleted, TriggerCheckpointEmit, "AWAITING_INPUT"},
		// TriggerExternalWait: wrong origin.
		{"externalwait_wrong_origin", persistence.TaskStatusLeased, persistence.TaskStatusAwaitingExternal, TriggerExternalWait, "AWAITING_EXTERNAL"},
		// TriggerLeaseExpired: wrong destination.
		{"leaseexpired_wrong_dest", persistence.TaskStatusLeased, persistence.TaskStatusCompleted, TriggerLeaseExpired, "QUEUED"},
		// TriggerLeaseExpired: origin must be LEASED or RUNNING (PENDING
		// → QUEUED hits the origin gate, not the destination gate, nor
		// the self-loop gate).
		{"leaseexpired_wrong_origin", persistence.TaskStatusPending, persistence.TaskStatusQueued, TriggerLeaseExpired, "LEASED/RUNNING"},
		// TriggerWaitChildren: wrong origin.
		{"waitchildren_wrong_origin", persistence.TaskStatusLeased, persistence.TaskStatusWaitingForChildren, TriggerWaitChildren, "RUNNING"},
		// TriggerChildrenDone: must land COMPLETED or FAILED, not QUEUED.
		{"childrendone_wrong_dest", persistence.TaskStatusWaitingForChildren, persistence.TaskStatusQueued, TriggerChildrenDone, "COMPLETED or FAILED"},
		// TriggerChildrenDone: wrong origin.
		{"childrendone_wrong_origin", persistence.TaskStatusRunning, persistence.TaskStatusCompleted, TriggerChildrenDone, "WAITING_FOR_CHILDREN"},
		// TriggerOperatorAnswer: wrong origin.
		{"answer_wrong_origin", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerOperatorAnswer, "AWAITING_INPUT"},
		// TriggerOperatorPause: wrong destination.
		{"pause_wrong_dest", persistence.TaskStatusRunning, persistence.TaskStatusCompleted, TriggerOperatorPause, "PAUSED"},
		// TriggerOperatorPause: cannot pause from PENDING/COMPLETED — the
		// validator requires Active or AwaitingInput. COMPLETED is
		// neither (it's neither active nor "awaiting input").
		{"pause_completed_rejected", persistence.TaskStatusCompleted, persistence.TaskStatusPaused, TriggerOperatorPause, "active or awaiting"},
		// TriggerOperatorResume: wrong origin.
		{"resume_wrong_origin", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerOperatorResume, "PAUSED → QUEUED"},
		// TriggerOperatorCancel: wrong destination.
		{"cancel_wrong_dest", persistence.TaskStatusRunning, persistence.TaskStatusFailed, TriggerOperatorCancel, "CANCELLED"},
		// TriggerOperatorCancel: terminal source rejected (FAILED is terminal — handled by the absorbing-state guard earlier).
		// TriggerOperatorClose: wrong destination.
		{"close_wrong_dest", persistence.TaskStatusCompleted, persistence.TaskStatusQueued, TriggerOperatorClose, "CLOSED"},
		// TriggerExternalEvent: wrong origin.
		{"externalevent_wrong_origin", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerExternalEvent, "AWAITING_EXTERNAL → QUEUED"},
		// TriggerExternalDeadline: wrong destination.
		{"externaldeadline_wrong_dest", persistence.TaskStatusAwaitingExternal, persistence.TaskStatusCompleted, TriggerExternalDeadline, "AWAITING_EXTERNAL → QUEUED"},
		// TriggerOperatorAmend / Directive: terminal source rejected.
		{"directive_terminal_source", persistence.TaskStatusFailed, persistence.TaskStatusQueued, TriggerOperatorDirective, "terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransition(tc.from, tc.to, tc.trigger)
			if err == nil {
				t.Fatalf("expected error; got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestStateMachine_AwaitingApproval covers the autonomy manual-approval
// surface (https://docs.vornik.io).
// Autonomy tasks created under requireApproval land in AWAITING_APPROVAL
// — a first-class awaiting-action status (so it surfaces in the inbox)
// that the operator resolves with approve (→ QUEUED) or reject
// (→ CANCELLED). Before this status existed they were parked in PENDING,
// which the scheduler never leased and no UI surfaced — they waited
// forever (operator report 2026-06-09).
func TestStateMachine_AwaitingApproval(t *testing.T) {
	t.Run("approve_requeues", func(t *testing.T) {
		if err := ValidateTransition(
			persistence.TaskStatusAwaitingApproval,
			persistence.TaskStatusQueued,
			TriggerOperatorApprove,
		); err != nil {
			t.Errorf("AWAITING_APPROVAL → QUEUED under approve must be allowed, got %v", err)
		}
	})

	t.Run("reject_cancels", func(t *testing.T) {
		if err := ValidateTransition(
			persistence.TaskStatusAwaitingApproval,
			persistence.TaskStatusCancelled,
			TriggerOperatorReject,
		); err != nil {
			t.Errorf("AWAITING_APPROVAL → CANCELLED under reject must be allowed, got %v", err)
		}
	})

	// Negative branches: approve/reject only fire from AWAITING_APPROVAL
	// and only to their one legal destination. In particular a stuck
	// PENDING task (the overloaded at-rest status) must NOT be approvable.
	neg := []struct {
		name    string
		from    persistence.TaskStatus
		to      persistence.TaskStatus
		trigger TransitionTrigger
		wantSub string
	}{
		{"approve_wrong_origin_pending", persistence.TaskStatusPending, persistence.TaskStatusQueued, TriggerOperatorApprove, "AWAITING_APPROVAL → QUEUED"},
		{"approve_wrong_origin_running", persistence.TaskStatusRunning, persistence.TaskStatusQueued, TriggerOperatorApprove, "AWAITING_APPROVAL → QUEUED"},
		{"approve_wrong_dest", persistence.TaskStatusAwaitingApproval, persistence.TaskStatusRunning, TriggerOperatorApprove, "AWAITING_APPROVAL → QUEUED"},
		{"reject_wrong_dest", persistence.TaskStatusAwaitingApproval, persistence.TaskStatusQueued, TriggerOperatorReject, "AWAITING_APPROVAL → CANCELLED"},
		{"reject_wrong_origin", persistence.TaskStatusRunning, persistence.TaskStatusCancelled, TriggerOperatorReject, "AWAITING_APPROVAL → CANCELLED"},
	}
	for _, tc := range neg {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransition(tc.from, tc.to, tc.trigger)
			if err == nil {
				t.Fatalf("expected error; got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err.Error(), tc.wantSub)
			}
		})
	}

	// Predicate placement: AWAITING_APPROVAL is an awaiting-action status
	// (inbox/badge surface), is NOT active (needs operator action before
	// it can run), and is NOT terminal.
	if !IsAwaitingInput(persistence.TaskStatusAwaitingApproval) {
		t.Error("AWAITING_APPROVAL must report awaiting-input (drives inbox/badge surface)")
	}
	if IsActiveTaskStatus(persistence.TaskStatusAwaitingApproval) {
		t.Error("AWAITING_APPROVAL is not active (needs operator approval before it can run)")
	}
	if IsTerminalTaskStatus(persistence.TaskStatusAwaitingApproval) {
		t.Error("AWAITING_APPROVAL must NOT be terminal")
	}
}
