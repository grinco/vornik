package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// recordTransitions wires a TransitionConditionalFunc that mimics the
// real conditional semantics against `task` and records every call
// (the transitionCall type is shared with lead_handoff_test.go).
// Bug-sweep follow-up 2026-06-04: the parent wake-up was converted
// from unconditional Update/UpdateStatus — which lost updates under
// concurrent child finalisers — to TransitionConditional gated on
// WAITING_FOR_CHILDREN; these tests pin the atomic write shape.
func recordTransitions(task *persistence.Task, calls *[]transitionCall) func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
	return func(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
		*calls = append(*calls, transitionCall{id: id, from: from, to: to, opts: opts})
		matched := false
		for _, s := range from {
			if task.Status == s {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
		task.Status = to
		if opts.Attempt > 0 {
			task.Attempt = opts.Attempt
		}
		if opts.LastError != nil {
			task.LastError = opts.LastError
		}
		if opts.LastErrorClass != nil {
			task.LastErrorClass = opts.LastErrorClass
		}
		return true, nil
	}
}

// TestCheckParentUnblock_RetriesParentWhenBudgetRemains — the
// parent-after-child failure path respects the parent's MaxAttempts.
// When a child fails and the parent has retry budget, the parent must
// be re-queued (Status=QUEUED, Attempt incremented) NOT terminally
// FAILED, with LastError + LastErrorClass=CHILD_FAILED stamped — all
// on ONE conditional transition gated on WAITING_FOR_CHILDREN so a
// concurrent child finaliser can't double-apply it.
func TestCheckParentUnblock_RetriesParentWhenBudgetRemains(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)

	parentID := "parent-1"
	failedChildID := "child-1"
	parent := &persistence.Task{
		ID:          parentID,
		Status:      persistence.TaskStatusWaitingForChildren,
		Attempt:     1,
		MaxAttempts: 3,
	}
	child := &persistence.Task{
		ID:           failedChildID,
		ParentTaskID: &parentID,
		Status:       persistence.TaskStatusFailed,
	}

	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			if id == parentID {
				return parent, nil
			}
			return nil, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return []*persistence.Task{child}, nil
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}

	e := &Executor{logger: logger, taskRepo: repo}
	e.checkParentUnblock(context.Background(), child)

	if len(calls) != 1 {
		t.Fatalf("TransitionConditional calls = %d, want exactly 1", len(calls))
	}
	call := calls[0]
	if len(call.from) != 1 || call.from[0] != persistence.TaskStatusWaitingForChildren {
		t.Errorf("from = %v, want [WAITING_FOR_CHILDREN] — the transition must be conditional", call.from)
	}
	if call.to != persistence.TaskStatusQueued {
		t.Errorf("to: got %s, want QUEUED (retry budget remained)", call.to)
	}
	if call.opts.Attempt != 2 {
		t.Errorf("opts.Attempt: got %d, want 2 (incremented from 1 on the same UPDATE)", call.opts.Attempt)
	}
	if call.opts.LastError == nil || !strings.Contains(*call.opts.LastError, failedChildID) {
		t.Errorf("LastError must reference failed child id; got %v", call.opts.LastError)
	}
	if call.opts.LastErrorClass == nil || *call.opts.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("LastErrorClass: got %v, want CHILD_FAILED", call.opts.LastErrorClass)
	}
	if parent.Status != persistence.TaskStatusQueued || parent.Attempt != 2 {
		t.Errorf("parent after transition = %s attempt %d, want QUEUED attempt 2", parent.Status, parent.Attempt)
	}

	got := buf.String()
	for _, want := range []string{
		`"parent_task_id":"parent-1"`,
		`"failed_child_task_ids":["child-1"]`,
		`"parent_attempt":1`,
		`"parent_max_attempts":3`,
		`"retry_budget_remaining":true`,
		`"decision_path":"checkParentUnblock.anyFailed"`,
		`"transitioned":true`,
		"parent re-queued for retry",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, got)
		}
	}
}

// TestCheckParentUnblock_TreatsClosedChildAsTerminal — CLOSED is an
// operator-confirmed terminal status; the parent must resume via one
// conditional WAITING_FOR_CHILDREN → QUEUED transition.
func TestCheckParentUnblock_TreatsClosedChildAsTerminal(t *testing.T) {
	parentID := "parent-closed"
	parent := &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	child := &persistence.Task{
		ID:           "child-closed",
		ParentTaskID: &parentID,
		Status:       persistence.TaskStatusClosed,
	}

	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return parent, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return []*persistence.Task{child}, nil
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.checkParentUnblock(context.Background(), child)

	if len(calls) != 1 || calls[0].to != persistence.TaskStatusQueued {
		t.Errorf("transitions = %+v, want exactly one → QUEUED", calls)
	}
}

// TestNotifyChildTerminal_DispatchesToParentSweep — the UI close
// path needs an entry point to drive the parent-unblock sweep when
// it transitions a child to CLOSED outside the executor's own flow.
// Verify NotifyChildTerminal loads the child and forwards to the
// unblock core (parent → QUEUED on the all-children-done path).
func TestNotifyChildTerminal_DispatchesToParentSweep(t *testing.T) {
	parentID := "parent-notify"
	parent := &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	child := &persistence.Task{
		ID:           "child-notify",
		ParentTaskID: &parentID,
		Status:       persistence.TaskStatusClosed,
	}
	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			if id == parent.ID {
				return parent, nil
			}
			if id == child.ID {
				return child, nil
			}
			return nil, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return []*persistence.Task{child}, nil
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.NotifyChildTerminal(context.Background(), child.ID)

	if len(calls) != 1 || calls[0].to != persistence.TaskStatusQueued {
		t.Errorf("transitions = %+v, want exactly one → QUEUED", calls)
	}

	// Empty childTaskID is a no-op — guards the close-failed branch
	// where the UI doesn't have a task ID to forward.
	e.NotifyChildTerminal(context.Background(), "")
}

// TestCheckParentUnblock_TerminalWhenBudgetExhausted — when the
// parent's retry budget is exhausted, terminal FAILED is correct
// and the LastError must still be stamped so operators see what
// went wrong.
func TestCheckParentUnblock_TerminalWhenBudgetExhausted(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := zerolog.New(buf)

	parentID := "parent-2"
	parent := &persistence.Task{
		ID:          parentID,
		Status:      persistence.TaskStatusWaitingForChildren,
		Attempt:     3,
		MaxAttempts: 3,
	}
	child := &persistence.Task{
		ID:           "child-2",
		ParentTaskID: &parentID,
		Status:       persistence.TaskStatusFailed,
	}

	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return parent, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return []*persistence.Task{child}, nil
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	e := &Executor{logger: logger, taskRepo: repo}
	e.checkParentUnblock(context.Background(), child)

	if len(calls) != 1 {
		t.Fatalf("TransitionConditional calls = %d, want exactly 1", len(calls))
	}
	call := calls[0]
	if call.to != persistence.TaskStatusFailed {
		t.Errorf("to: got %s, want FAILED (retry budget exhausted)", call.to)
	}
	if call.opts.Attempt != 0 {
		t.Errorf("opts.Attempt: got %d, want 0 (attempt preserved on terminal fail)", call.opts.Attempt)
	}
	if call.opts.LastErrorClass == nil || *call.opts.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("LastErrorClass: got %v, want CHILD_FAILED", call.opts.LastErrorClass)
	}
	if parent.Status != persistence.TaskStatusFailed || parent.Attempt != 3 {
		t.Errorf("parent after transition = %s attempt %d, want FAILED attempt 3", parent.Status, parent.Attempt)
	}

	got := buf.String()
	if !strings.Contains(got, `"retry_budget_remaining":false`) {
		t.Errorf("expected retry_budget_remaining:false; log:\n%s", got)
	}
	if strings.Contains(got, "parent re-queued for retry") {
		t.Errorf("must not log re-queued when budget exhausted; log:\n%s", got)
	}
}

// TestCheckParentUnblock_ConcurrentFinalisers_SingleWake is the
// regression for the 2026-06-04 lost-update finding: the last two
// children of a fan-out finishing together both observe allDone and
// both try to wake the parent. With the conditional transition,
// exactly ONE wake succeeds — the parent's Attempt is bumped once and
// it is queued once. Pre-fix both writers applied unconditional
// Updates: Attempt could double-increment (skipping a retry) and the
// parent could be re-queued twice.
func TestCheckParentUnblock_ConcurrentFinalisers_SingleWake(t *testing.T) {
	parentID := "parent-race"
	parent := &persistence.Task{
		ID:          parentID,
		Status:      persistence.TaskStatusWaitingForChildren,
		Attempt:     1,
		MaxAttempts: 3,
	}
	childA := &persistence.Task{ID: "child-a", ParentTaskID: &parentID, Status: persistence.TaskStatusFailed}
	childB := &persistence.Task{ID: "child-b", ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted}

	// Both finalisers read the SAME stale parent snapshot (status
	// WAITING_FOR_CHILDREN, attempt 1) — the exact interleave of the
	// race. The conditional transition must let only one win.
	staleSnapshot := *parent
	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			snap := staleSnapshot
			return &snap, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return []*persistence.Task{childA, childB}, nil
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}

	// Sequential here, but with the stale snapshot both callers pass
	// the status guard and reach the write — modelling the concurrent
	// interleave deterministically.
	e.checkParentUnblock(context.Background(), childA)
	e.checkParentUnblock(context.Background(), childB)

	if len(calls) != 2 {
		t.Fatalf("TransitionConditional calls = %d, want 2 (both finalisers attempt the wake)", len(calls))
	}
	if parent.Attempt != 2 {
		t.Errorf("parent.Attempt = %d, want 2 — the losing finaliser must NOT double-increment (pre-fix it did)", parent.Attempt)
	}
	if parent.Status != persistence.TaskStatusQueued {
		t.Errorf("parent.Status = %s, want QUEUED", parent.Status)
	}
}

// TestUnblockParent_ZeroChildrenIsNoOp — guards the self-heal entry
// point added with the delegation-window fix (2026-06-04): a
// WAITING_FOR_CHILDREN parent whose children are not visible via
// GetChildren (e.g. a cross-project callee tracked in the CPC ledger)
// must NOT be re-queued on a vacuous "all done" conclusion.
func TestUnblockParent_ZeroChildrenIsNoOp(t *testing.T) {
	parentID := "parent-cpc"
	parent := &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	var calls []transitionCall
	repo := &mocks.MockTaskRepository{
		GetFunc: func(ctx context.Context, id string) (*persistence.Task, error) {
			return parent, nil
		},
		GetChildrenFunc: func(ctx context.Context, parentID string) ([]*persistence.Task, error) {
			return nil, nil // no in-tree children visible
		},
		TransitionConditionalFunc: recordTransitions(parent, &calls),
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.unblockParentIfChildrenDone(context.Background(), parentID)

	if len(calls) != 0 {
		t.Errorf("transitions = %+v, want none — zero visible children must not wake the parent", calls)
	}
	if parent.Status != persistence.TaskStatusWaitingForChildren {
		t.Errorf("parent status = %s, want WAITING_FOR_CHILDREN (untouched)", parent.Status)
	}
}
