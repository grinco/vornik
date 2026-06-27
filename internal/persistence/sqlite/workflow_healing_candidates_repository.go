package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowHealingCandidateRepository is the SQLite stub for the
// Self-Healing Workflow Genome v1 candidate ledger (migration 87).
// Same Postgres-only discipline as the trigger + override ledgers —
// the API/UI nil-check upstream so the stub mostly serves to satisfy
// the interface in the test build.
type WorkflowHealingCandidateRepository struct {
	_ *sql.DB
}

// NewWorkflowHealingCandidateRepository returns the stub.
func NewWorkflowHealingCandidateRepository(_ *sql.DB) *WorkflowHealingCandidateRepository {
	return &WorkflowHealingCandidateRepository{}
}

// ErrSQLiteHealingCandidatesUnsupported surfaces from every stub
// method. Same shape as the overrides stub.
var ErrSQLiteHealingCandidatesUnsupported = errors.New(
	"workflow_healing_candidates: SQLite backend not supported in v1 (use Postgres)")

func (r *WorkflowHealingCandidateRepository) Insert(context.Context, *persistence.HealingCandidate) error {
	return ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) Get(context.Context, string) (*persistence.HealingCandidate, error) {
	return nil, ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) List(context.Context, persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) SetStatus(context.Context, string, persistence.HealingCandidateStatus) error {
	return ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) BeginTrial(context.Context, string) (bool, error) {
	return false, ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) Promote(context.Context, string, string) error {
	return ErrSQLiteHealingCandidatesUnsupported
}

func (r *WorkflowHealingCandidateRepository) Reject(context.Context, string) error {
	return ErrSQLiteHealingCandidatesUnsupported
}
