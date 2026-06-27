package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// OperatorProfileRepository is the SQLite stub. Single-process
// deployments don't strictly need persistent operator profiles
// — the in-memory channel session state already covers
// preferences within one daemon lifetime. The stub keeps the
// interface satisfied so the dispatcher's nil-check stays
// uniform across backends.
//
// Future: if SQLite operators want persistent profiles across
// restarts, this stub gets a real impl mirroring the Postgres
// path. The schema is portable enough.
type OperatorProfileRepository struct {
	_ *sql.DB
}

// NewOperatorProfileRepository returns the stub.
func NewOperatorProfileRepository(db *sql.DB) *OperatorProfileRepository {
	return &OperatorProfileRepository{}
}

// Get always reports ErrNotFound. Callers fall back to default
// dispatcher prompt (no operator-profile block injected).
func (r *OperatorProfileRepository) Get(_ context.Context, _ string) (*persistence.OperatorProfile, error) {
	return nil, persistence.ErrNotFound
}

// Upsert is a no-op.
func (r *OperatorProfileRepository) Upsert(_ context.Context, _ *persistence.OperatorProfile) error {
	return nil
}

// Delete is a no-op for the same reason as Upsert.
func (r *OperatorProfileRepository) Delete(_ context.Context, _ string) error {
	return nil
}

// List returns an empty slice. Single-process deployments
// don't persist profiles via this stub; the UI shows the
// operator section empty rather than erroring.
func (r *OperatorProfileRepository) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	return nil, nil
}
