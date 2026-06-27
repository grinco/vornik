package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingTrialRepository is the SQLite stub for the
// Self-Healing Workflow Genome v1 trial ledger (migration 88). Same
// Postgres-only discipline as the candidate ledger.
type WorkflowHealingTrialRepository struct {
	_ *sql.DB
}

// NewWorkflowHealingTrialRepository returns the stub.
func NewWorkflowHealingTrialRepository(_ *sql.DB) *WorkflowHealingTrialRepository {
	return &WorkflowHealingTrialRepository{}
}

// ErrSQLiteHealingTrialsUnsupported surfaces from every stub method.
var ErrSQLiteHealingTrialsUnsupported = errors.New(
	"workflow_healing_trials: SQLite backend not supported in v1 (use Postgres)")

func (r *WorkflowHealingTrialRepository) Insert(context.Context, *persistence.HealingTrial) error {
	return ErrSQLiteHealingTrialsUnsupported
}

func (r *WorkflowHealingTrialRepository) Get(context.Context, string) (*persistence.HealingTrial, error) {
	return nil, ErrSQLiteHealingTrialsUnsupported
}

func (r *WorkflowHealingTrialRepository) ListByCandidate(context.Context, string) ([]*persistence.HealingTrial, error) {
	return nil, ErrSQLiteHealingTrialsUnsupported
}

func (r *WorkflowHealingTrialRepository) Finish(context.Context, string, persistence.HealingTrialVerdict, string, string, string) error {
	return ErrSQLiteHealingTrialsUnsupported
}
