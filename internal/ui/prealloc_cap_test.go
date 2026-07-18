package ui

import "testing"

// TestActiveTasksPreallocCap is the regression guard for the CodeQL
// go/uncontrolled-allocation-size finding on the dashboard "active tasks"
// pre-allocation. `limit` comes from ?limit=N via parsePageSize, which
// whitelists it to PageSizeOptions (an equality check CodeQL does not read as
// an upper bound), so the make-site clamps with min(limit, maxActiveTasksPrealloc).
func TestActiveTasksPreallocCap(t *testing.T) {
	const huge = 1 << 30
	if got := min(huge, maxActiveTasksPrealloc); got != maxActiveTasksPrealloc {
		t.Errorf("min(%d, %d) = %d, want clamp to %d", huge, maxActiveTasksPrealloc, got, maxActiveTasksPrealloc)
	}
	if got := min(20, maxActiveTasksPrealloc); got != 20 {
		t.Errorf("legitimate size 20 must pass through, got %d", got)
	}
	// The cap must never truncate a value parsePageSize can legitimately
	// return, or a real request would be under-allocated (and, worse, the
	// clamp would change behaviour rather than just bound the pathological
	// range).
	for _, opt := range PageSizeOptions {
		if opt > maxActiveTasksPrealloc {
			t.Errorf("PageSizeOptions entry %d exceeds prealloc cap %d", opt, maxActiveTasksPrealloc)
		}
	}
}
