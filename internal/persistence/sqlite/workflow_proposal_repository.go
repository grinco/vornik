package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// WorkflowProposalRepository is the SQLite stub for the
// memetic-workflows architect (Slice 2). The architect agent only
// runs against Postgres-backed deployments — single-process SQLite
// deployments aren't the audience for self-evolving workflows, and
// the partial unique index that enforces the per-workflow rate
// limit at the DB layer doesn't exist on SQLite either.
//
// Stub semantics:
//   - Insert returns ErrNotFound so the admin propose endpoint
//     can fail-soft to 503 ("workflow architect not available on
//     this backend"), matching the same pattern CrossProjectCalls
//     uses.
//   - Get / List always return empty / ErrNotFound.
//   - Decide / MarkApplied / MarkRolledBack are no-ops.
type WorkflowProposalRepository struct {
	_ *sql.DB
}

// NewWorkflowProposalRepository returns the stub.
func NewWorkflowProposalRepository(_ *sql.DB) *WorkflowProposalRepository {
	return &WorkflowProposalRepository{}
}

// Insert returns ErrNotFound to signal "this backend doesn't
// support the architect"; consumers nil-check / errors.Is to
// distinguish from a genuine row-missing case.
func (r *WorkflowProposalRepository) Insert(_ context.Context, _ *persistence.WorkflowProposal) error {
	return persistence.ErrNotFound
}

// Get always returns ErrNotFound on the SQLite branch.
func (r *WorkflowProposalRepository) Get(_ context.Context, _ string) (*persistence.WorkflowProposal, error) {
	return nil, persistence.ErrNotFound
}

// List returns an empty slice. Empty (not nil) so the JSON
// serialiser writes `[]` rather than `null` — keeps the admin
// API contract stable.
func (r *WorkflowProposalRepository) List(_ context.Context, _ persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return []*persistence.WorkflowProposal{}, nil
}

// Decide is a no-op. Operator-decision flows never reach this
// repo on SQLite (the architect never inserted).
func (r *WorkflowProposalRepository) Decide(_ context.Context, _ string, _ persistence.WorkflowProposalStatus, _, _ string) error {
	return nil
}

// MarkApplied is a no-op for the same reason.
func (r *WorkflowProposalRepository) MarkApplied(_ context.Context, _, _ string) error {
	return nil
}

// MarkRolledBack is a no-op for the same reason.
func (r *WorkflowProposalRepository) MarkRolledBack(_ context.Context, _, _ string) error {
	return nil
}

// UpdateProposalYAML is a no-op on the SQLite branch (no proposals
// are ever stored). Mirrors the other mutators.
func (r *WorkflowProposalRepository) UpdateProposalYAML(_ context.Context, _, _, _ string) error {
	return nil
}
