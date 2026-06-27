package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestSweepStuckWaitingForChildren_UnblocksReadyParent — the
// startup convergence sweep introduced 2026-05-26 catches parents
// stranded in WAITING_FOR_CHILDREN whose children have all reached
// terminal status. Live evidence: T-a8e1 stuck after T-0833 closed
// via closure_request before the parent-unblock fix landed. Even
// with the closure_request fix in place, daemon restarts can drop
// in-flight unblock calls, so this sweep is the convergence
// backstop that keeps state from drifting over crashes.
//
// Wires both repo interfaces — sweep uses persistTaskRepo (broad,
// has List); checkParentUnblock uses the narrow executor.TaskRepository.
// MockTaskRepository satisfies both.
func TestSweepStuckWaitingForChildren_UnblocksReadyParent(t *testing.T) {
	parentID := "task_parent"
	childID := "task_child"
	parent := &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	child := &persistence.Task{
		ID:           childID,
		Status:       persistence.TaskStatusCompleted,
		ParentTaskID: &parentID,
	}

	var mu sync.Mutex
	statusOfParent := func() persistence.TaskStatus {
		mu.Lock()
		defer mu.Unlock()
		return parent.Status
	}

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			// Sweep should filter by Status=WAITING_FOR_CHILDREN.
			if f.Status == nil || *f.Status != persistence.TaskStatusWaitingForChildren {
				t.Errorf("sweep called List with wrong filter: %+v", f)
			}
			return []*persistence.Task{parent}, nil
		},
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			mu.Lock()
			defer mu.Unlock()
			if id == parentID {
				cp := *parent
				return &cp, nil
			}
			return nil, persistence.ErrNotFound
		},
		GetChildrenFunc: func(_ context.Context, pid string) ([]*persistence.Task, error) {
			if pid != parentID {
				return nil, nil
			}
			return []*persistence.Task{child}, nil
		},
		// The unblock core writes via TransitionConditional since the
		// 2026-06-04 lost-update race fix (UpdateStatus before) —
		// mimic the conditional semantics so the sweep's effect is
		// observable on `parent`.
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if id != parentID {
				return false, nil
			}
			for _, s := range from {
				if parent.Status == s {
					parent.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}

	e := &Executor{
		taskRepo:        repo, // narrow path (used by checkParentUnblock)
		persistTaskRepo: repo, // broad path (used by sweep's List)
		logger:          zerolog.Nop(),
	}

	e.sweepStuckWaitingForChildren(context.Background())

	// Parent transitioned to QUEUED — children all COMPLETED, no
	// failures, so the parent resumes for its next step.
	assert.Equal(t, persistence.TaskStatusQueued, statusOfParent(),
		"parent must transition WAITING_FOR_CHILDREN → QUEUED when all children are terminal")
}

// TestSweepStuckWaitingForChildren_LeavesInflightChildrenAlone — when
// a stuck parent has a child still in flight (RUNNING / QUEUED /
// LEASED / etc.), the sweep must NOT wake the parent. Otherwise we'd
// race the in-flight child's terminal transition.
func TestSweepStuckWaitingForChildren_LeavesInflightChildrenAlone(t *testing.T) {
	parentID := "task_parent_inflight"
	parent := &persistence.Task{
		ID:     parentID,
		Status: persistence.TaskStatusWaitingForChildren,
	}
	doneChild := &persistence.Task{ID: "c1", Status: persistence.TaskStatusCompleted, ParentTaskID: &parentID}
	runningChild := &persistence.Task{ID: "c2", Status: persistence.TaskStatusRunning, ParentTaskID: &parentID}

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{parent}, nil
		},
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == parentID {
				cp := *parent
				return &cp, nil
			}
			return nil, persistence.ErrNotFound
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return []*persistence.Task{doneChild, runningChild}, nil
		},
		UpdateStatusFunc: func(_ context.Context, _ string, st persistence.TaskStatus) error {
			t.Errorf("UpdateStatus must NOT fire when a child is still in flight (tried to set %s)", st)
			return nil
		},
		UpdateFunc: func(_ context.Context, _ *persistence.Task) error {
			t.Errorf("Update must NOT fire when a child is still in flight")
			return nil
		},
	}

	e := &Executor{
		taskRepo:        repo,
		persistTaskRepo: repo,
		logger:          zerolog.Nop(),
	}
	e.sweepStuckWaitingForChildren(context.Background())
	// Parent unchanged.
	assert.Equal(t, persistence.TaskStatusWaitingForChildren, parent.Status)
}

// TestSweepStuckWaitingForChildren_NilRepoIsNoop — defensive: when
// persistTaskRepo isn't wired (e.g. legacy executor without the
// conversational lifecycle Option), the sweep is a clean no-op.
// Mirrors the conditional-wiring pattern other Phase-25 paths use.
func TestSweepStuckWaitingForChildren_NilRepoIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	// Should not panic. Nothing to assert beyond that.
	e.sweepStuckWaitingForChildren(context.Background())
}

// TestSweepStuckWaitingForChildren_ListErrorIsLoggedNotFatal — a
// DB blip on the List query at startup must not crash recovery.
// The sweep logs and returns; the next restart retries naturally.
func TestSweepStuckWaitingForChildren_ListErrorIsLoggedNotFatal(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("connection refused")
		},
	}
	e := &Executor{
		taskRepo:        repo,
		persistTaskRepo: repo,
		logger:          zerolog.Nop(),
	}
	// Must not panic; the logger swallows the warning.
	require.NotPanics(t, func() {
		e.sweepStuckWaitingForChildren(context.Background())
	})
}

// TestSweepStuckWaitingForChildren_EmptyListSilentNoop — the
// common case: no parents are stuck. Sweep should make exactly one
// List call and return without further repo activity.
func TestSweepStuckWaitingForChildren_EmptyListSilentNoop(t *testing.T) {
	var listCalls int
	var otherCalls int
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			listCalls++
			return nil, nil
		},
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			otherCalls++
			return nil, persistence.ErrNotFound
		},
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			otherCalls++
			return nil, nil
		},
	}
	e := &Executor{
		taskRepo:        repo,
		persistTaskRepo: repo,
		logger:          zerolog.Nop(),
	}
	e.sweepStuckWaitingForChildren(context.Background())
	assert.Equal(t, 1, listCalls, "sweep should make exactly one List call")
	assert.Equal(t, 0, otherCalls, "sweep should not call Get/GetChildren when the list is empty")
}
