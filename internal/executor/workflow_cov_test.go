package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestWorkflowCov_NotifyChildTerminalGuards — NotifyChildTerminal is a
// no-op for a nil receiver, a nil taskRepo, an empty child ID, a child
// load error, and a missing (nil) child. None must panic or reach the
// parent-unblock core.
func TestWorkflowCov_NotifyChildTerminalGuards(t *testing.T) {
	// nil receiver
	var nilExec *Executor
	nilExec.NotifyChildTerminal(context.Background(), "c1")

	// nil taskRepo
	(&Executor{logger: zerolog.Nop()}).NotifyChildTerminal(context.Background(), "c1")

	// child Get error — must not call GetChildren / unblock core
	errRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("db error")
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			t.Fatal("GetChildren must not run when the child load errors")
			return nil, nil
		},
	}
	(&Executor{logger: zerolog.Nop(), taskRepo: errRepo}).NotifyChildTerminal(context.Background(), "c1")

	// nil child (Get returns nil,nil)
	nilChildRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, nil
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			t.Fatal("GetChildren must not run when the child is nil")
			return nil, nil
		},
	}
	(&Executor{logger: zerolog.Nop(), taskRepo: nilChildRepo}).NotifyChildTerminal(context.Background(), "c1")
}

// TestWorkflowCov_UnblockParentGetError — when the parent load errors,
// the sweep bails before reading children (the early-return guard).
func TestWorkflowCov_UnblockParentGetError(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("parent gone")
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			t.Fatal("GetChildren must not run when the parent load errors")
			return nil, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.unblockParentIfChildrenDone(context.Background(), "parent-x") // must not panic
}

// TestWorkflowCov_UnblockParentNotWaiting — a parent that isn't in
// WAITING_FOR_CHILDREN is left alone (the status guard). No children
// query, no transition.
func TestWorkflowCov_UnblockParentNotWaiting(t *testing.T) {
	parent := &persistence.Task{ID: "p", Status: persistence.TaskStatusRunning}
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return parent, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			calls++
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.unblockParentIfChildrenDone(context.Background(), "p")
	assert.Equal(t, 0, calls, "non-waiting parent must not be transitioned")
}

// TestWorkflowCov_UnblockParentGetChildrenError — when the children
// query errors, the sweep bails (can't conclude all-done from a failed
// read).
func TestWorkflowCov_UnblockParentGetChildrenError(t *testing.T) {
	parent := &persistence.Task{ID: "p", Status: persistence.TaskStatusWaitingForChildren}
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return parent, nil
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return nil, errors.New("children query failed")
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			calls++
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.unblockParentIfChildrenDone(context.Background(), "p")
	assert.Equal(t, 0, calls, "children-query error must short-circuit before any transition")
}

// TestWorkflowCov_UnblockParentNotAllDone — when at least one child is
// still in-flight (non-terminal), the parent stays put (the !allDone
// arm). No transition fires.
func TestWorkflowCov_UnblockParentNotAllDone(t *testing.T) {
	parentID := "p"
	parent := &persistence.Task{ID: parentID, Status: persistence.TaskStatusWaitingForChildren}
	children := []*persistence.Task{
		{ID: "c1", ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted},
		{ID: "c2", ParentTaskID: &parentID, Status: persistence.TaskStatusRunning}, // still in-flight
	}
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return parent, nil
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return children, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			calls++
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.unblockParentIfChildrenDone(context.Background(), parentID)
	assert.Equal(t, 0, calls, "a still-running child must keep the parent waiting")
}

// TestWorkflowCov_CheckParentUnblockNoParent — checkParentUnblock is a
// no-op when the task has no parent (nil or empty ParentTaskID).
func TestWorkflowCov_CheckParentUnblockNoParent(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			t.Fatal("no parent lookup should occur when ParentTaskID is unset")
			return nil, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.checkParentUnblock(context.Background(), &persistence.Task{ID: "orphan"}) // nil ParentTaskID
	empty := ""
	e.checkParentUnblock(context.Background(), &persistence.Task{ID: "orphan2", ParentTaskID: &empty})
}
