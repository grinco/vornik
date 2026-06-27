package ui

import "context"

// BlackBoxTraceService is the narrow seam the admin blackbox UI
// handlers drive. The concrete implementation (*blackbox.Service)
// lives in the EE adapter and is wired via WithBlackBoxService.
//
// AssembleCached returns an opaque any value (a *blackbox.Trace from
// the EE side) that the template reads through directly. The CE code
// never inspects the concrete type — it is passed straight into the
// template data struct.
type BlackBoxTraceService interface {
	// AssembleCached returns the assembled trace for a task.
	// Returns (nil, false, contracts.ErrBlackBoxTaskNotFound) when the
	// task has no audit data. The trace value is opaque to CE — the
	// enterprise adapter owns the concrete type.
	AssembleCached(ctx context.Context, taskID string) (trace any, cached bool, err error)

	// Compare produces a scorecard from two assembled traces.
	// Both arguments must be the values previously returned by
	// AssembleCached on this same adapter instance. Returns (nil, nil)
	// when either argument cannot be asserted to the concrete type.
	Compare(a, b any) (scorecard any, err error)
}
