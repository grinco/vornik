package registry

import (
	"sync"
	"testing"
)

// TestSetProjectAutonomyEnabled_NoRaceWithReaders is the regression for
// the 2026-06-04 bug sweep: SetProjectAutonomyEnabled mutated
// project.Autonomy.Enabled in place while GetProject / ListProjects
// hand out the same *Project pointers and the autonomy manager reads
// that field without holding r.mu — a data race.
//
// Run under `go test -race` (the default for `make test-unit`): pre-fix
// the writer's in-place store and the reader's lock-free load on the
// same field trip the race detector. Post-fix the writer swaps in a
// fresh struct under the lock, so readers always see an immutable
// snapshot.
func TestSetProjectAutonomyEnabled_NoRaceWithReaders(t *testing.T) {
	reg := New() // configDir empty -> in-memory only, no disk I/O
	reg.projects["p1"] = &Project{ID: "p1"}

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: flip the flag repeatedly.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = reg.SetProjectAutonomyEnabled("p1", i%2 == 0)
		}
	}()

	// Reader: read the field the autonomy manager reads, the same way
	// (via GetProject, lock-free field access on the returned pointer).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if p := reg.GetProject("p1"); p != nil {
				_ = p.Autonomy.Enabled
			}
		}
	}()

	wg.Wait()
}
