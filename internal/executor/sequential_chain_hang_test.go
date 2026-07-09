package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestCheckParentUnblock_SequentialChainFailureCancelsTail is the regression
// for the SEQUENTIAL-chain hang observed after fixing the delegation bug
// (incident task_20260709102613_79c570a868fefedb, headmatch #34): the parent's
// decompose spawned 7 issue-subtask children as a SEQUENTIAL chain
// (child[i] DEPENDS_ON child[i-1]). When child 2 FAILED, children 3..7 stayed
// QUEUED forever (their predecessor never reached COMPLETED, so they were never
// leasable), unblockParentIfChildrenDone never saw allDone, and the parent hung
// in WAITING_FOR_CHILDREN indefinitely.
//
// The fix: a child that terminates without success cancels its blocked
// dependents (the SEQUENTIAL tail) so the cohort reaches all-terminal and the
// parent fails per the "any subtask failure → FAILED, no PR" contract.
func TestCheckParentUnblock_SequentialChainFailureCancelsTail(t *testing.T) {
	const parentID = "parent-1"

	parent := &persistence.Task{ID: parentID, Status: persistence.TaskStatusWaitingForChildren, Attempt: 1, MaxAttempts: 1}
	child2 := &persistence.Task{ID: "child-2", ParentTaskID: strptr(parentID), Status: persistence.TaskStatusFailed}
	child3 := &persistence.Task{ID: "child-3", ParentTaskID: strptr(parentID), Status: persistence.TaskStatusQueued}
	child4 := &persistence.Task{ID: "child-4", ParentTaskID: strptr(parentID), Status: persistence.TaskStatusQueued}
	child1 := &persistence.Task{ID: "child-1", ParentTaskID: strptr(parentID), Status: persistence.TaskStatusCompleted}

	byID := map[string]*persistence.Task{
		parentID: parent, "child-1": child1, "child-2": child2, "child-3": child3, "child-4": child4,
	}
	// SEQUENTIAL edges: 1←2←3←4 (GetDependents(x) = tasks that DEPENDS_ON x).
	dependents := map[string][]*persistence.Task{
		"child-1": {child2}, "child-2": {child3}, "child-3": {child4}, "child-4": {},
	}

	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) { return byID[id], nil },
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return []*persistence.Task{child1, child2, child3, child4}, nil
		},
		GetDependentsFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			return dependents[id], nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
			task := byID[id]
			if task == nil {
				return false, nil
			}
			for _, s := range from {
				if task.Status == s {
					task.Status = to
					if opts.LastError != nil {
						task.LastError = opts.LastError
					}
					if opts.LastErrorClass != nil {
						task.LastErrorClass = opts.LastErrorClass
					}
					return true, nil
				}
			}
			return false, nil
		},
	}

	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.checkParentUnblock(context.Background(), child2)

	// The blocked tail must be cancelled...
	if child3.Status != persistence.TaskStatusCancelled {
		t.Errorf("child-3: got %s, want CANCELLED (blocked on failed child-2)", child3.Status)
	}
	if child4.Status != persistence.TaskStatusCancelled {
		t.Errorf("child-4: got %s, want CANCELLED (transitively blocked)", child4.Status)
	}
	if child3.LastErrorClass == nil || *child3.LastErrorClass != persistence.TaskFailureClassChildFailed {
		t.Errorf("child-3 LastErrorClass: got %v, want CHILD_FAILED", child3.LastErrorClass)
	}
	// ...the already-COMPLETED sibling must be left intact...
	if child1.Status != persistence.TaskStatusCompleted {
		t.Errorf("child-1: got %s, want COMPLETED (must not be touched)", child1.Status)
	}
	// ...and with the tail now terminal the parent must fail (no retry budget).
	if parent.Status != persistence.TaskStatusFailed {
		t.Errorf("parent: got %s, want FAILED — the chain hang must resolve to a clean terminal", parent.Status)
	}
}

// TestCancelBlockedDependents_NoDependentsIsNoOp guards the PARALLEL/FAN_OUT
// case: a failed child with no DEPENDS_ON dependents cancels nothing (those
// cohorts finish on their own; the parent still fails via anyFailed).
func TestCancelBlockedDependents_NoDependentsIsNoOp(t *testing.T) {
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetDependentsFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			calls++
			return true, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.cancelBlockedDependents(context.Background(), "child-x", "reason", map[string]bool{"child-x": true})
	if calls != 0 {
		t.Fatalf("expected no cancellations when there are no dependents, got %d", calls)
	}
}

func strptr(s string) *string { return &s }
