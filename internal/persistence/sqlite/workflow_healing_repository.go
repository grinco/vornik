package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingTriggerRepository is the SQLite stub for the
// Black Box Phase B trigger ledger. The detector relies on
// Postgres's partial unique index to dedup open triggers — the
// equivalent SQLite construct (CREATE UNIQUE INDEX ... WHERE)
// works but the rest of the Phase B surface (telemetry rollup,
// leader election) requires Postgres anyway. The stub satisfies
// the interface so storage.Open builds a complete Repositories
// on the SQLite branch (test build); every method returns
// ErrSQLiteHealingTriggersUnsupported so callers surface a
// clear "not configured" message.
type WorkflowHealingTriggerRepository struct {
	_ *sql.DB
}

// NewWorkflowHealingTriggerRepository returns the stub. Same
// constructor shape as the postgres impl so storage.Build
// branches on driver without an adapter.
func NewWorkflowHealingTriggerRepository(_ *sql.DB) *WorkflowHealingTriggerRepository {
	return &WorkflowHealingTriggerRepository{}
}

// ErrSQLiteHealingTriggersUnsupported surfaces from every stub
// method. The detector + handler both nil-check upstream, so
// this error rarely lands in practice; it's defensive.
var ErrSQLiteHealingTriggersUnsupported = errors.New(
	"workflow_healing_triggers: SQLite backend not supported in v1 (use Postgres)")

func (r *WorkflowHealingTriggerRepository) Insert(context.Context, *persistence.HealingTrigger) error {
	return ErrSQLiteHealingTriggersUnsupported
}
func (r *WorkflowHealingTriggerRepository) Get(context.Context, string) (*persistence.HealingTrigger, error) {
	return nil, ErrSQLiteHealingTriggersUnsupported
}
func (r *WorkflowHealingTriggerRepository) List(context.Context, persistence.HealingTriggerListFilter) ([]*persistence.HealingTrigger, error) {
	return nil, ErrSQLiteHealingTriggersUnsupported
}
func (r *WorkflowHealingTriggerRepository) Dismiss(context.Context, string) error {
	return ErrSQLiteHealingTriggersUnsupported
}
func (r *WorkflowHealingTriggerRepository) MarkGenerated(context.Context, string, string) error {
	return ErrSQLiteHealingTriggersUnsupported
}
