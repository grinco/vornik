package autonomy

import (
	"context"
	"time"

	"vornik.io/vornik/internal/registry"
)

// export_test.go exposes one internal hook for tests living in the
// external autonomy_test package. Production callers never see this —
// Go's _test.go convention keeps it out of the regular build.

// TickBacklogForTest exposes the unexported tickBacklog to
// backlog_deposit_integration_test.go, which moved to package
// autonomy_test when internal/api started importing internal/autonomy
// (Task 5, GetAutonomyHealth's autonomy.FeedObservations call) — that
// import made internal/api's own reverse test-only import of
// internal/autonomy (needed to drive the real HTTP deposit handler)
// cyclic from inside package autonomy itself. Moving the test out
// breaks the cycle; this hook is what it needs from the package under
// test that isn't otherwise exported.
func (m *Manager) TickBacklogForTest(ctx context.Context, p *registry.Project, now time.Time) error {
	return m.tickBacklog(ctx, p, now)
}
