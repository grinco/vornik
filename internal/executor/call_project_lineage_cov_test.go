package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// lineageCov_getErrRepo errors (non-NotFound) on the parent lookup.
type lineageCov_getErrRepo struct{ err error }

func (r *lineageCov_getErrRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return nil, r.err
}

// lineageCov_chainRepo serves a fixed parent chain by ID so the
// walker can exceed lineageWalkHardLimit (cycle) → Truncated=true.
type lineageCov_chainRepo struct {
	byID map[string]*persistence.Task
}

func (r *lineageCov_chainRepo) Get(_ context.Context, id string) (*persistence.Task, error) {
	if t, ok := r.byID[id]; ok {
		return t, nil
	}
	return nil, persistence.ErrNotFound
}

// TestWalkCallerLineageCov_NilCaller covers the nil-caller guard.
func TestWalkCallerLineageCov_NilCaller(t *testing.T) {
	out, err := walkCallerLineage(context.Background(), &lineageCov_chainRepo{}, nil)
	if err != nil {
		t.Fatalf("nil caller should not error: %v", err)
	}
	if out.Depth != 0 || len(out.AncestorProjects) != 0 {
		t.Errorf("nil caller should yield empty lineage, got %+v", out)
	}
}

// TestWalkCallerLineageCov_GetError covers the non-NotFound error
// branch: a parent Get failure propagates.
func TestWalkCallerLineageCov_GetError(t *testing.T) {
	parent := "p1"
	caller := &persistence.Task{ID: "c1", ProjectID: "proj-a", ParentTaskID: &parent}
	_, err := walkCallerLineage(context.Background(), &lineageCov_getErrRepo{err: errors.New("db down")}, caller)
	if err == nil || !contains(err.Error(), "walk lineage") {
		t.Fatalf("expected walk-lineage error, got %v", err)
	}
}

// TestWalkCallerLineageCov_Truncated covers the hard-limit branch:
// a parent_task_id cycle (a→b→a) is walked until lineageWalkHardLimit
// and flagged Truncated.
func TestWalkCallerLineageCov_Truncated(t *testing.T) {
	a := "a"
	b := "b"
	repo := &lineageCov_chainRepo{byID: map[string]*persistence.Task{
		"a": {ID: "a", ProjectID: "proj-a", ParentTaskID: &b},
		"b": {ID: "b", ProjectID: "proj-b", ParentTaskID: &a},
	}}
	caller := &persistence.Task{ID: "a", ProjectID: "proj-a", ParentTaskID: &b}
	out, err := walkCallerLineage(context.Background(), repo, caller)
	if err != nil {
		t.Fatalf("cyclic chain should be bounded, not error: %v", err)
	}
	if !out.Truncated {
		t.Error("expected Truncated=true when the parent chain cycles past the hard limit")
	}
}
