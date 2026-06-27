package service

// Adapter for api.WorkflowTelemetry — bridges
// *workflowtelemetry.Service (returns *Rollup) to the api package's
// narrow interface (returns any). Kept in the service container so
// the api package stays free of an import on workflowtelemetry.

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/workflowtelemetry"
)

type workflowTelemetryAdapter struct {
	svc *workflowtelemetry.Service
}

func newWorkflowTelemetryAdapter(db *sql.DB) *workflowTelemetryAdapter {
	return &workflowTelemetryAdapter{svc: workflowtelemetry.NewService(db)}
}

func (a *workflowTelemetryAdapter) ForWorkflow(ctx context.Context, workflowID string, since time.Time) (any, error) {
	if a == nil || a.svc == nil {
		return nil, nil
	}
	return a.svc.ForWorkflow(ctx, workflowID, since)
}
