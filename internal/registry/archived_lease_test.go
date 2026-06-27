package registry

import (
	"testing"
)

// TestArchivedProjectIDs_ListsOnlyArchived pins the scheduler-
// facing helper that drives the archived-project lease hard-guard.
// Mixed-state fixtures so a regression that returns every project
// would show up as a length mismatch immediately.
func TestArchivedProjectIDs_ListsOnlyArchived(t *testing.T) {
	r := New()
	r.active.projects = map[string]*Project{
		"alpha":   {ID: "alpha"}, // active
		"beta":    {ID: "beta", Lifecycle: ProjectLifecycle{Status: "archived"}},
		"gamma":   {ID: "gamma", Lifecycle: ProjectLifecycle{Status: ""}}, // explicit-active
		"delta":   {ID: "delta", Lifecycle: ProjectLifecycle{Status: "archived"}},
		"epsilon": {ID: "epsilon", Lifecycle: ProjectLifecycle{Status: "active"}}, // typed-active
	}
	r.projects = r.active.projects

	got := r.ArchivedProjectIDs()
	want := map[string]bool{"beta": true, "delta": true}
	if len(got) != len(want) {
		t.Fatalf("ArchivedProjectIDs len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("ArchivedProjectIDs returned non-archived %q", id)
		}
	}
}

// TestArchivedProjectIDs_EmptyRegistry — a brand-new daemon
// returns nil/empty rather than panicking.
func TestArchivedProjectIDs_EmptyRegistry(t *testing.T) {
	r := New()
	if got := r.ArchivedProjectIDs(); len(got) != 0 {
		t.Errorf("empty registry should return empty list; got %v", got)
	}
}
