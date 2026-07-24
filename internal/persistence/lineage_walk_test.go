package persistence

import (
	"context"
	"testing"
)

// These tests reuse fakeTaskLister + strptr from request_grouping_test.go.

func TestResolveLineageWithCompleteness_CleanRoot(t *testing.T) {
	lister := &fakeTaskLister{byID: map[string]*Task{
		"c": {ID: "c", ParentTaskID: strptr("b")},
		"b": {ID: "b", ParentTaskID: strptr("a")},
		"a": {ID: "a"}, // root: nil parent
	}}
	ids, outcome, err := ResolveLineageWithCompleteness(context.Background(), lister, "c", 25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if outcome != WalkOutcomeCleanRoot {
		t.Fatalf("outcome = %v, want clean_root", outcome)
	}
	if len(ids) != 3 || ids[0] != "c" {
		t.Fatalf("ids = %v, want [c b a]", ids)
	}
}

func TestResolveLineageWithCompleteness_MissingParent(t *testing.T) {
	lister := &fakeTaskLister{byID: map[string]*Task{
		"c": {ID: "c", ParentTaskID: strptr("gone")},
	}}
	ids, outcome, err := ResolveLineageWithCompleteness(context.Background(), lister, "c", 25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if outcome != WalkOutcomeMissingParent {
		t.Fatalf("outcome = %v, want missing_parent", outcome)
	}
	if len(ids) != 1 || ids[0] != "c" {
		t.Fatalf("ids = %v, want [c] (writing task still included)", ids)
	}
}

func TestResolveLineageWithCompleteness_Cycle(t *testing.T) {
	lister := &fakeTaskLister{byID: map[string]*Task{
		"c": {ID: "c", ParentTaskID: strptr("b")},
		"b": {ID: "b", ParentTaskID: strptr("c")}, // cycle
	}}
	_, outcome, err := ResolveLineageWithCompleteness(context.Background(), lister, "c", 25)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if outcome != WalkOutcomeCycle {
		t.Fatalf("outcome = %v, want cycle", outcome)
	}
}

func TestResolveLineageWithCompleteness_DepthExhausted(t *testing.T) {
	lister := &fakeTaskLister{byID: map[string]*Task{
		"d": {ID: "d", ParentTaskID: strptr("c")},
		"c": {ID: "c", ParentTaskID: strptr("b")},
		"b": {ID: "b", ParentTaskID: strptr("a")},
		"a": {ID: "a"},
	}}
	_, outcome, err := ResolveLineageWithCompleteness(context.Background(), lister, "d", 2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if outcome != WalkOutcomeDepthExhausted {
		t.Fatalf("outcome = %v, want depth_exhausted", outcome)
	}
}
