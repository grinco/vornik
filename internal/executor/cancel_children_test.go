package executor

import (
	"context"
	"sort"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestCancelChildren_CascadesRecursively is the regression test for the
// downward-cascade gap: cancelling a parent left its route/delegation/
// checkpoint children (and their descendants) RUNNING/QUEUED. CancelChildren
// must walk the parent_task_id tree and cancel every non-terminal descendant
// via a WAITING_FOR_CHILDREN-inclusive conditional transition, while leaving
// already-terminal tasks untouched.
func TestCancelChildren_CascadesRecursively(t *testing.T) {
	parentID := "parent-1"
	// Tree:
	//   parent-1 (already CANCELLED — the just-cancelled target)
	//     ├─ child-run   (RUNNING)
	//     ├─ child-wfc   (WAITING_FOR_CHILDREN, an intermediate parent)
	//     │    └─ grand   (LEASED)
	//     └─ child-done  (COMPLETED — terminal, must be skipped)
	tasks := map[string]*persistence.Task{
		"parent-1":   {ID: "parent-1", Status: persistence.TaskStatusCancelled},
		"child-run":  {ID: "child-run", ParentTaskID: &parentID, Status: persistence.TaskStatusRunning},
		"child-wfc":  {ID: "child-wfc", ParentTaskID: &parentID, Status: persistence.TaskStatusWaitingForChildren},
		"grand":      {ID: "grand", Status: persistence.TaskStatusLeased},
		"child-done": {ID: "child-done", ParentTaskID: &parentID, Status: persistence.TaskStatusCompleted},
	}
	wfcID := "child-wfc"
	tasks["grand"].ParentTaskID = &wfcID

	childrenOf := map[string][]string{
		"parent-1":  {"child-run", "child-wfc", "child-done"},
		"child-wfc": {"grand"},
	}

	repo := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			var out []*persistence.Task
			for _, cid := range childrenOf[id] {
				out = append(out, tasks[cid])
			}
			return out, nil
		},
		TransitionConditionalFunc: func(_ context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			task := tasks[id]
			for _, s := range from {
				if task.Status == s {
					task.Status = to
					return true, nil
				}
			}
			return false, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.CancelChildren(context.Background(), parentID)

	// Every non-terminal descendant is now CANCELLED.
	for _, id := range []string{"child-run", "child-wfc", "grand"} {
		if tasks[id].Status != persistence.TaskStatusCancelled {
			t.Errorf("%s = %s, want CANCELLED", id, tasks[id].Status)
		}
	}
	// The already-terminal child is untouched.
	if tasks["child-done"].Status != persistence.TaskStatusCompleted {
		t.Errorf("child-done = %s, want COMPLETED (terminal, must not be overwritten)", tasks["child-done"].Status)
	}
}

// TestCancelChildren_ConditionalCoversWaitingForChildren pins that the
// cascade's conditional transition includes WAITING_FOR_CHILDREN in its
// from-set — an intermediate parent in that state must be cancellable, else
// its own subtree would be stranded.
func TestCancelChildren_ConditionalCoversWaitingForChildren(t *testing.T) {
	parentID := "p"
	child := &persistence.Task{ID: "c", ParentTaskID: &parentID, Status: persistence.TaskStatusWaitingForChildren}
	var seenFrom []persistence.TaskStatus
	repo := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, id string) ([]*persistence.Task, error) {
			if id == parentID {
				return []*persistence.Task{child}, nil
			}
			return nil, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			seenFrom = from
			child.Status = to
			return true, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.CancelChildren(context.Background(), parentID)

	got := make([]string, 0, len(seenFrom))
	for _, s := range seenFrom {
		got = append(got, string(s))
	}
	sort.Strings(got)
	want := map[string]bool{"WAITING_FOR_CHILDREN": true, "RUNNING": true, "QUEUED": true, "LEASED": true, "PENDING": true}
	for _, g := range got {
		delete(want, g)
	}
	if len(want) != 0 {
		t.Errorf("conditional from-set %v missing statuses %v", got, want)
	}
}

// TestCancelChildren_NilSafeAndNoChildren — a childless (or nil) target is a
// clean no-op, not a panic.
func TestCancelChildren_NilSafeAndNoChildren(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CancelChildren panicked: %v", r)
		}
	}()
	repo := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	e := &Executor{logger: zerolog.Nop(), taskRepo: repo}
	e.CancelChildren(context.Background(), "lonely")

	var nilExec *Executor
	nilExec.CancelChildren(context.Background(), "x") // must not panic
}
