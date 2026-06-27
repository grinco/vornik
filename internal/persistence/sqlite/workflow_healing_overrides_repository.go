package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingOverrideRepository is the SQLite stub for the
// Phase B operator-override surface (migration 81). Same Postgres-
// only discipline as the trigger ledger — the detector + UI both
// nil-check upstream so the stub mostly serves to satisfy the
// interface in the test build.
type WorkflowHealingOverrideRepository struct {
	_ *sql.DB
}

// NewWorkflowHealingOverrideRepository returns the stub.
func NewWorkflowHealingOverrideRepository(_ *sql.DB) *WorkflowHealingOverrideRepository {
	return &WorkflowHealingOverrideRepository{}
}

// ErrSQLiteHealingOverridesUnsupported surfaces from every stub
// method. Same shape as the triggers stub.
var ErrSQLiteHealingOverridesUnsupported = errors.New(
	"workflow_healing_overrides: SQLite backend not supported in v1 (use Postgres)")

func (r *WorkflowHealingOverrideRepository) Upsert(context.Context, *persistence.HealingTriggerOverride) error {
	return ErrSQLiteHealingOverridesUnsupported
}

func (r *WorkflowHealingOverrideRepository) Get(context.Context, string, string, persistence.HealingTriggerClass) (*persistence.HealingTriggerOverride, error) {
	return nil, ErrSQLiteHealingOverridesUnsupported
}

func (r *WorkflowHealingOverrideRepository) List(context.Context, int) ([]*persistence.HealingTriggerOverride, error) {
	return nil, ErrSQLiteHealingOverridesUnsupported
}

func (r *WorkflowHealingOverrideRepository) Delete(context.Context, string, string, persistence.HealingTriggerClass) error {
	return ErrSQLiteHealingOverridesUnsupported
}
