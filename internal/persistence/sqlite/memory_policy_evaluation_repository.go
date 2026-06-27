package sqlite

// SQLite implementation of MemoryPolicyEvaluationRepository.
// The Policy-Aware Memory Firewall is documented as Postgres-
// only in v1; this stub exists so the SQLite backend can
// satisfy the Repositories struct's invariant ("every field
// non-nil"). Writes are no-ops, reads return empty.
//
// Operators running SQLite see the firewall's CLI / UI
// surfaces return empty data rather than panic. When the
// SQLite path needs a real impl in the future, it lands here.

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
)

type MemoryPolicyEvaluationRepository struct {
	db *sql.DB
}

// NewMemoryPolicyEvaluationRepository wires the SQLite stub.
// db is unused today; kept in the signature so a real impl
// can land without an API break.
func NewMemoryPolicyEvaluationRepository(db *sql.DB) *MemoryPolicyEvaluationRepository {
	return &MemoryPolicyEvaluationRepository{db: db}
}

// BatchInsert is a no-op on SQLite — the audit table doesn't
// exist there (migration 80 is Postgres-shaped). Writes log
// nothing and return nil so the firewall audit writer's
// flusher goroutine doesn't backpressure.
func (r *MemoryPolicyEvaluationRepository) BatchInsert(_ context.Context, _ []memoryfirewall.EvaluationRow) error {
	return nil
}

// ListRecent returns an empty slice on SQLite.
func (r *MemoryPolicyEvaluationRepository) ListRecent(_ context.Context, _, _ string, _ time.Time, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}

// ListByDigest returns an empty slice on SQLite (firewall is
// Postgres-only in v1). Present so the SQLite backend satisfies the
// MemoryPolicyEvaluationRepository interface after the digest
// proof-verifier landed (drift-mitigation §8.3).
func (r *MemoryPolicyEvaluationRepository) ListByDigest(_ context.Context, _ string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}

// ListByChunk returns an empty slice on SQLite (firewall is Postgres-only).
func (r *MemoryPolicyEvaluationRepository) ListByChunk(_ context.Context, _ string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}
