package api

import (
	"context"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
)

// BlackBoxTraceService is the narrow seam the admin blackbox trace
// endpoints drive. The concrete implementation (blackbox.Service)
// lives in EE and is wired via WithBlackBoxService when the deployment
// is Postgres-backed.
//
// AssembleCached returns an opaque any value (a *blackbox.Trace from
// the EE side) that is JSON-marshaled directly by the CE handler.
// CE code never inspects the trace's internal fields.
type BlackBoxTraceService interface {
	// AssembleCached returns the assembled trace for a task.
	// Returns (nil, false, contracts.ErrBlackBoxTaskNotFound) when the
	// task has no audit data. The trace value is opaque to CE — the
	// enterprise adapter owns the concrete type.
	AssembleCached(ctx context.Context, taskID string) (trace any, cached bool, err error)

	// Compare produces a scorecard from two assembled traces.
	// Both arguments must be the values previously returned by
	// AssembleCached on this same adapter instance (the adapter will
	// type-assert back to its internal type). Returns (nil, nil) when
	// either argument cannot be asserted.
	Compare(a, b any) (scorecard any, err error)
}

// BlackBoxReplayEngine is the narrow seam the admin blackbox replay
// endpoint drives. The concrete implementation (blackbox.Engine)
// lives in EE and is wired via WithBlackBoxEngine when the deployment
// supports counterfactual replay.
type BlackBoxReplayEngine interface {
	// Apply creates a counterfactual replay task from the given plan.
	// Returns (nil, contracts.ErrBlackBoxVariableNotImplemented) for
	// unimplemented variables; (nil, contracts.ErrBlackBoxMissingOriginal)
	// when the original task is missing; (task, stampErr) when the replay
	// succeeded but the stamp-after-success returned a warning error.
	Apply(ctx context.Context, plan contracts.BlackBoxReplayPlan) (newTask *persistence.Task, err error)
}

// BlackBoxReplayPlan is a local alias for contracts.BlackBoxReplayPlan
// — kept for backward compatibility within this package so handler code
// reads naturally. Both types are identical; the CE layer uses
// contracts.BlackBoxReplayPlan at the interface boundary.
type BlackBoxReplayPlan = contracts.BlackBoxReplayPlan
