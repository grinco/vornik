package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// OperatorProfileRepository implements
// persistence.OperatorProfileRepository against PostgreSQL.
// Upsert uses ON CONFLICT (operator_id) DO UPDATE so the
// dispatcher's per-turn write (when the future
// update_operator_profile tool ships) is one round trip.
type OperatorProfileRepository struct {
	db DBTX
}

// NewOperatorProfileRepository constructs the repo over db.
func NewOperatorProfileRepository(db DBTX) *OperatorProfileRepository {
	return &OperatorProfileRepository{db: db}
}

// Get returns the profile row. ErrNotFound when no row exists.
func (r *OperatorProfileRepository) Get(ctx context.Context, operatorID string) (*persistence.OperatorProfile, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator_profile: operator_id required")
	}
	const q = `
SELECT operator_id, structured, notes, created_at, updated_at
FROM operator_profile
WHERE operator_id = $1`
	row := r.db.QueryRowContext(ctx, q, operatorID)
	var p persistence.OperatorProfile
	if err := row.Scan(&p.OperatorID, &p.Structured, &p.Notes, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("operator_profile: get: %w", err)
	}
	return &p, nil
}

// Upsert inserts or updates the operator's profile. Empty
// Structured coerces to `{}` so the JSONB NOT NULL constraint
// is satisfied.
func (r *OperatorProfileRepository) Upsert(ctx context.Context, p *persistence.OperatorProfile) error {
	if p == nil || p.OperatorID == "" {
		return fmt.Errorf("operator_profile: operator_id required")
	}
	structured := p.Structured
	if len(structured) == 0 {
		structured = []byte("{}")
	}
	const q = `
INSERT INTO operator_profile (operator_id, structured, notes, created_at, updated_at)
VALUES ($1, $2, $3, NOW(), NOW())
ON CONFLICT (operator_id) DO UPDATE
SET structured = EXCLUDED.structured,
    notes = EXCLUDED.notes,
    updated_at = NOW()`
	if _, err := r.db.ExecContext(ctx, q, p.OperatorID, structured, p.Notes); err != nil {
		return fmt.Errorf("operator_profile: upsert: %w", err)
	}
	return nil
}

// Delete removes the profile. Idempotent — no error when the
// row doesn't exist.
func (r *OperatorProfileRepository) Delete(ctx context.Context, operatorID string) error {
	const q = `DELETE FROM operator_profile WHERE operator_id = $1`
	if _, err := r.db.ExecContext(ctx, q, operatorID); err != nil {
		return fmt.Errorf("operator_profile: delete: %w", err)
	}
	return nil
}

// List returns recent profiles ordered by updated_at DESC.
// limit <= 0 → default 50; values > 500 cap at 500. The UI's
// operator list page consumes this; the dispatcher's per-turn
// read uses Get directly so this method's row count doesn't
// affect chat latency.
func (r *OperatorProfileRepository) List(ctx context.Context, limit int) ([]*persistence.OperatorProfile, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	const q = `
SELECT operator_id, structured, notes, created_at, updated_at
FROM operator_profile
ORDER BY updated_at DESC
LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("operator_profile: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.OperatorProfile
	for rows.Next() {
		var p persistence.OperatorProfile
		if err := rows.Scan(&p.OperatorID, &p.Structured, &p.Notes, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("operator_profile: list scan: %w", err)
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}
