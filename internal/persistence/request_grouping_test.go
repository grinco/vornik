package persistence

import (
	"context"
	"errors"
	"strconv"
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

// TestResolveRequestRootsWithCompleteness_CleanRoot — a clean chain to a
// genuine ParentTaskID==nil root reports clean_root and resolves to that
// root. (LLD 2026-07-22 agent-write policy; the ONLY complete outcome.)
func TestResolveRequestRootsWithCompleteness_CleanRoot(t *testing.T) {
	root := &Task{ID: "root"}
	child := &Task{ID: "c1", ParentTaskID: strptr("root")}
	lister := &fakeTaskLister{byID: map[string]*Task{"root": root, "c1": child}}

	roots, outcome, err := ResolveRequestRootsWithCompleteness(
		context.Background(), lister, []*Task{child}, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRootsWithCompleteness: %v", err)
	}
	if outcome["c1"] != WalkOutcomeCleanRoot {
		t.Errorf("outcome = %q, want clean_root", outcome["c1"])
	}
	if roots["c1"] == nil || roots["c1"].ID != "root" {
		t.Errorf("root = %+v, want the nil-parent root task", roots["c1"])
	}
}

// TestResolveRequestRootsWithCompleteness_MissingParent — a dangling
// ParentTaskID (parent row absent/deleted) reports missing_parent, NOT
// clean_root. This is the C1/C2 privilege-escalation guard: the
// authorization caller must refuse, never treat the last-found task as a
// root.
func TestResolveRequestRootsWithCompleteness_MissingParent(t *testing.T) {
	// non-USER-rooted chain with a deleted parent above a USER intermediate:
	// user mode must NOT grant on the USER intermediate.
	userMid := &Task{ID: "mid", ParentTaskID: strptr("gone"), CreationSource: TaskCreationSourceUser}
	child := &Task{ID: "c1", ParentTaskID: strptr("mid")}
	lister := &fakeTaskLister{byID: map[string]*Task{"mid": userMid, "c1": child}}

	roots, outcome, err := ResolveRequestRootsWithCompleteness(
		context.Background(), lister, []*Task{child}, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRootsWithCompleteness: %v", err)
	}
	if outcome["c1"] != WalkOutcomeMissingParent {
		t.Errorf("outcome = %q, want missing_parent (incomplete ⇒ refuse)", outcome["c1"])
	}
	// The last-found node is the USER intermediate — proving that a naive
	// "root is USER" check on this map WOULD wrongly grant; the outcome is
	// what protects the gate.
	if roots["c1"] == nil || roots["c1"].ID != "mid" {
		t.Errorf("last-found = %+v, want the USER intermediate (mid)", roots["c1"])
	}
}

// TestResolveRequestRootsWithCompleteness_CycleDistinctFromDepth — a
// ParentTaskID cycle is detected explicitly and reported as cycle (not
// depth_exhausted), terminating BEFORE the depth bound. Guards review
// I-R4: cycle and depth_exhausted are distinct outcomes.
func TestResolveRequestRootsWithCompleteness_CycleDistinctFromDepth(t *testing.T) {
	a := &Task{ID: "a", ParentTaskID: strptr("b")}
	b := &Task{ID: "b", ParentTaskID: strptr("a")}
	lister := &fakeTaskLister{byID: map[string]*Task{"a": a, "b": b}}

	_, outcome, err := ResolveRequestRootsWithCompleteness(
		context.Background(), lister, []*Task{a}, 25)
	if err != nil {
		t.Fatalf("ResolveRequestRootsWithCompleteness: %v", err)
	}
	if outcome["a"] != WalkOutcomeCycle {
		t.Errorf("outcome = %q, want cycle (detected early, distinct from depth_exhausted)", outcome["a"])
	}
	// And the cheap-cycle detection means we did NOT walk all 25 levels.
	if lister.callCount >= 25 {
		t.Errorf("cycle should stop early, got %d List calls", lister.callCount)
	}
}

// TestResolveRequestRootsWithCompleteness_DepthExhausted — a chain longer
// than maxDepth (no cycle, no missing parent) reports depth_exhausted:
// the walk ran out of budget without reaching a nil-parent root, so the
// root is unknown ⇒ incomplete.
func TestResolveRequestRootsWithCompleteness_DepthExhausted(t *testing.T) {
	// Build a linear chain n0<-n1<-...<-n10, all present, deeper than maxDepth=3.
	byID := map[string]*Task{}
	for i := 0; i <= 10; i++ {
		id := "n" + strconv.Itoa(i)
		task := &Task{ID: id}
		if i > 0 {
			task.ParentTaskID = strptr("n" + strconv.Itoa(i-1))
		}
		byID[id] = task
	}
	lister := &fakeTaskLister{byID: byID}

	_, outcome, err := ResolveRequestRootsWithCompleteness(
		context.Background(), lister, []*Task{byID["n10"]}, 3)
	if err != nil {
		t.Fatalf("ResolveRequestRootsWithCompleteness: %v", err)
	}
	if outcome["n10"] != WalkOutcomeDepthExhausted {
		t.Errorf("outcome = %q, want depth_exhausted", outcome["n10"])
	}
}

// TestResolveRequestRootsWithCompleteness_PropagatesListError — a repo
// failure aborts the walk and returns the error (fail-closed for the
// authorization caller), never a half-resolved map.
func TestResolveRequestRootsWithCompleteness_PropagatesListError(t *testing.T) {
	child := &Task{ID: "c1", ParentTaskID: strptr("root")}
	roots, outcome, err := ResolveRequestRootsWithCompleteness(
		context.Background(), erroringTaskLister{}, []*Task{child}, 0)
	if err == nil {
		t.Fatal("expected the repo error to propagate")
	}
	if roots != nil || outcome != nil {
		t.Errorf("expected nil maps on error, got roots=%v outcome=%v", roots, outcome)
	}
}

// TestResolveRequestRoots_WrapperMatchesCompleteness — the byte-identical
// regression (review I-R3): ResolveRequestRoots (the shipped signature the
// attention-queue caller uses) must return exactly the roots that
// ResolveRequestRootsWithCompleteness resolves, for a realistic acyclic
// multi-level tree. Proves the wrapper is behaviour-preserving so the
// grouping caller is untouched.
func TestResolveRequestRoots_WrapperMatchesCompleteness(t *testing.T) {
	root := &Task{ID: "root"}
	c1 := &Task{ID: "c1", ParentTaskID: strptr("root")}
	c2 := &Task{ID: "c2", ParentTaskID: strptr("root")}
	gc := &Task{ID: "gc1", ParentTaskID: strptr("c1")}
	byID := map[string]*Task{"root": root, "c1": c1, "c2": c2, "gc1": gc}
	inputs := []*Task{c1, c2, gc}

	wrapperListers := &fakeTaskLister{byID: byID}
	wrapped, err := ResolveRequestRoots(context.Background(), wrapperListers, inputs, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRoots: %v", err)
	}
	fullLister := &fakeTaskLister{byID: byID}
	full, outcome, err := ResolveRequestRootsWithCompleteness(context.Background(), fullLister, inputs, 0)
	if err != nil {
		t.Fatalf("ResolveRequestRootsWithCompleteness: %v", err)
	}
	if len(wrapped) != len(full) {
		t.Fatalf("root-map size differs: wrapper=%d full=%d", len(wrapped), len(full))
	}
	for id, w := range wrapped {
		if full[id] == nil || full[id].ID != w.ID {
			t.Errorf("%s: wrapper root %q != full root %+v", id, w.ID, full[id])
		}
		if outcome[id] != WalkOutcomeCleanRoot {
			t.Errorf("%s: expected clean_root on an acyclic tree, got %q", id, outcome[id])
		}
	}
}
