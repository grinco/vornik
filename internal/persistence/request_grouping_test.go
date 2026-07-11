package persistence

import (
	"context"
	"errors"
	"testing"
)

// fakeTaskLister is a minimal, call-counting TaskLister fake — deliberately
// NOT the full TaskRepository interface, proving ResolveRequestRoots only
// needs List (so callers/tests don't have to stub every TaskRepository
// method to exercise the ancestor-walk).
type fakeTaskLister struct {
	byID      map[string]*Task
	callCount int
}

func (f *fakeTaskLister) List(_ context.Context, filter TaskFilter) ([]*Task, error) {
	f.callCount++
	out := make([]*Task, 0, len(filter.IDs))
	for _, id := range filter.IDs {
		if t, ok := f.byID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func strptr(s string) *string { return &s }

// TestResolveRequestRoots_BatchesPerLevelNotPerTask is the load-bearing
// assertion from the Outcome Inbox design (§5.3, review finding 2): a
// parent with several children (and one grandchild, for a 2-level walk)
// must resolve via ONE List call per depth level, not one per task.
func TestResolveRequestRoots_BatchesPerLevelNotPerTask(t *testing.T) {
	root := &Task{ID: "root"}
	child1 := &Task{ID: "c1", ParentTaskID: strptr("root")}
	child2 := &Task{ID: "c2", ParentTaskID: strptr("root")}
	grandchild := &Task{ID: "gc1", ParentTaskID: strptr("c1")}

	lister := &fakeTaskLister{byID: map[string]*Task{
		"root": root,
		"c1":   child1,
	}}

	got, err := ResolveRequestRoots(context.Background(), lister, []*Task{child1, child2, grandchild}, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}

	// Depth of the deepest chain (gc1 -> c1 -> root) is 2 levels, so
	// exactly 2 List calls resolve all 3 input tasks — never 3+ (which
	// a per-task walk would produce).
	if lister.callCount != 2 {
		t.Errorf("List call count = %d, want 2 (batched per level, not per task)", lister.callCount)
	}

	for _, want := range []struct{ id, rootID string }{
		{"c1", "root"},
		{"c2", "root"},
		{"gc1", "root"},
	} {
		got, ok := got[want.id]
		if !ok {
			t.Fatalf("missing resolution for %s", want.id)
		}
		if got.ID != want.rootID {
			t.Errorf("%s resolved to %s, want %s", want.id, got.ID, want.rootID)
		}
	}
}

// TestResolveRequestRoots_TaskWithNoParentIsItsOwnRoot — a root-level
// task (ParentTaskID nil) must resolve to itself with zero List calls.
func TestResolveRequestRoots_TaskWithNoParentIsItsOwnRoot(t *testing.T) {
	root := &Task{ID: "solo"}
	lister := &fakeTaskLister{byID: map[string]*Task{}}

	got, err := ResolveRequestRoots(context.Background(), lister, []*Task{root}, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}
	if got["solo"] != root {
		t.Errorf("expected solo task to resolve to itself, got %+v", got["solo"])
	}
	if lister.callCount != 0 {
		t.Errorf("expected no List calls for a parentless task, got %d", lister.callCount)
	}
}

// TestResolveRequestRoots_MissingParentRowStopsAtLastKnown — a dangling
// ParentTaskID (parent row deleted) must not error; the walk stops at
// the last resolved task instead.
func TestResolveRequestRoots_MissingParentRowStopsAtLastKnown(t *testing.T) {
	orphan := &Task{ID: "orphan", ParentTaskID: strptr("ghost")}
	lister := &fakeTaskLister{byID: map[string]*Task{}} // "ghost" never resolves

	got, err := ResolveRequestRoots(context.Background(), lister, []*Task{orphan}, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}
	if got["orphan"] != orphan {
		t.Errorf("expected orphan to resolve to itself when parent is missing, got %+v", got["orphan"])
	}
}

// TestResolveRequestRoots_CycleIsBoundedByMaxDepth — a corrupt
// ParentTaskID cycle must not spin forever; the walk terminates within
// the configured maxDepth.
func TestResolveRequestRoots_CycleIsBoundedByMaxDepth(t *testing.T) {
	a := &Task{ID: "a", ParentTaskID: strptr("b")}
	b := &Task{ID: "b", ParentTaskID: strptr("a")}
	lister := &fakeTaskLister{byID: map[string]*Task{"a": a, "b": b}}

	got, err := ResolveRequestRoots(context.Background(), lister, []*Task{a}, 3)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}
	if lister.callCount > 3 {
		t.Errorf("expected the walk to stop within maxDepth=3 List calls, got %d", lister.callCount)
	}
	if _, ok := got["a"]; !ok {
		t.Fatalf("expected a resolution for the cyclic chain's input task")
	}
}

// erroringTaskLister always fails List, to prove ResolveRequestRoots
// propagates a repo error instead of swallowing it.
type erroringTaskLister struct{}

func (erroringTaskLister) List(_ context.Context, _ TaskFilter) ([]*Task, error) {
	return nil, errors.New("boom")
}

// TestResolveRequestRoots_PropagatesListError — a List failure surfaces
// to the caller rather than returning a half-resolved map.
func TestResolveRequestRoots_PropagatesListError(t *testing.T) {
	child := &Task{ID: "c1", ParentTaskID: strptr("root")}
	_, err := ResolveRequestRoots(context.Background(), erroringTaskLister{}, []*Task{child}, 0)
	if err == nil {
		t.Fatal("expected the repo error to propagate")
	}
}

// TestResolveRequestRoots_EmptyInput — no tasks in, empty map out, no
// List calls.
func TestResolveRequestRoots_EmptyInput(t *testing.T) {
	lister := &fakeTaskLister{byID: map[string]*Task{}}
	got, err := ResolveRequestRoots(context.Background(), lister, nil, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %+v", got)
	}
	if lister.callCount != 0 {
		t.Errorf("expected no List calls, got %d", lister.callCount)
	}
}
