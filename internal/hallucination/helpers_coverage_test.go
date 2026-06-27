package hallucination

import (
	"testing"
)

// TestGroundingContext_MergeKnownIDs — operator-driven enrichment
// of the context with registry-derived IDs after the
// audit/artifact build. Pin all three sets so a future refactor
// doesn't accidentally drop one.
func TestGroundingContext_MergeKnownIDs(t *testing.T) {
	gc := &GroundingContext{}
	gc.MergeKnownIDs(
		[]string{"proj-1", "proj-2"},
		[]string{"task-a", "task-b"},
		[]string{"artifact.md"},
	)
	if _, ok := gc.KnownProjectIDs["proj-1"]; !ok {
		t.Errorf("KnownProjectIDs missing proj-1: %+v", gc.KnownProjectIDs)
	}
	if _, ok := gc.KnownTaskIDs["task-b"]; !ok {
		t.Errorf("KnownTaskIDs missing task-b: %+v", gc.KnownTaskIDs)
	}
	if _, ok := gc.KnownArtifactNames["artifact.md"]; !ok {
		t.Errorf("KnownArtifactNames missing artifact.md: %+v", gc.KnownArtifactNames)
	}

	// Second merge accumulates, doesn't replace.
	gc.MergeKnownIDs([]string{"proj-3"}, []string{"task-c"}, []string{"second.md"})
	if len(gc.KnownProjectIDs) != 3 {
		t.Errorf("merge should accumulate: %+v", gc.KnownProjectIDs)
	}
}

// TestGroundingContext_MergeKnownIDs_NilSliceArgs — defensive
// shape. Passing nil for any of the three slices must NOT panic.
func TestGroundingContext_MergeKnownIDs_NilSliceArgs(t *testing.T) {
	gc := &GroundingContext{}
	gc.MergeKnownIDs(nil, nil, nil)
	if gc.KnownProjectIDs == nil || gc.KnownTaskIDs == nil || gc.KnownArtifactNames == nil {
		t.Error("merge with nil slices should still allocate the maps")
	}
}
