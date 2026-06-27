package sqlite

import (
	"context"
	"database/sql"

	"vornik.io/vornik/internal/persistence"
)

// ProfileUseAuditRepository is the SQLite stub. Single-process
// deployments rarely need the audit surface — profile use is
// visible in the live conversation as it happens, and the
// chat-side journal carries enough breadcrumbs. The stub keeps
// the dispatcher's nil-check uniform across backends.
type ProfileUseAuditRepository struct {
	_ *sql.DB
}

// NewProfileUseAuditRepository returns the stub.
func NewProfileUseAuditRepository(db *sql.DB) *ProfileUseAuditRepository {
	return &ProfileUseAuditRepository{}
}

// Insert is a no-op.
func (r *ProfileUseAuditRepository) Insert(_ context.Context, _ *persistence.ProfileUseAudit) error {
	return nil
}

// ListForOperator returns the empty slice.
func (r *ProfileUseAuditRepository) ListForOperator(_ context.Context, _ string, _ persistence.ProfileUseAuditQuery) ([]*persistence.ProfileUseAudit, error) {
	return nil, nil
}

// DeleteAllForOperator is a no-op.
func (r *ProfileUseAuditRepository) DeleteAllForOperator(_ context.Context, _ string) error {
	return nil
}
