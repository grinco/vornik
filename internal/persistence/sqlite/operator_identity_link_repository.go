package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// OperatorIdentityLinkRepository is the SQLite stub. Mirrors
// OperatorProfileRepository's "single-process deployments don't
// need cross-process persistence" rationale — the dispatcher's
// canonical-id resolver no-ops to "speaker id IS the operator
// id" when no links exist, which is what every Get below returns.
type OperatorIdentityLinkRepository struct {
	_ *sql.DB
}

// NewOperatorIdentityLinkRepository returns the stub.
func NewOperatorIdentityLinkRepository(db *sql.DB) *OperatorIdentityLinkRepository {
	return &OperatorIdentityLinkRepository{}
}

// Get always returns ErrNotFound. Resolver treats this as
// "speaker id is its own canonical id".
func (r *OperatorIdentityLinkRepository) Get(_ context.Context, _ string) (*persistence.OperatorIdentityLink, error) {
	return nil, persistence.ErrNotFound
}

// ListForOperator always returns the empty slice.
func (r *OperatorIdentityLinkRepository) ListForOperator(_ context.Context, _ string) ([]*persistence.OperatorIdentityLink, error) {
	return nil, nil
}

// Upsert is a no-op.
func (r *OperatorIdentityLinkRepository) Upsert(_ context.Context, _ *persistence.OperatorIdentityLink) error {
	return nil
}

// Delete is a no-op.
func (r *OperatorIdentityLinkRepository) Delete(_ context.Context, _ string) error {
	return nil
}

// DeleteAllForOperator is a no-op.
func (r *OperatorIdentityLinkRepository) DeleteAllForOperator(_ context.Context, _ string) error {
	return nil
}
