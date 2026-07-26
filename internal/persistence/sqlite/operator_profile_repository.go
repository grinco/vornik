package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// OperatorProfileRepository implements durable per-operator profiles on
// SQLite. Community Edition commonly runs single-process SQLite, but profile
// memory must still survive restarts and must never acknowledge a write that
// was silently discarded.
type OperatorProfileRepository struct {
	db *sql.DB
}

// NewOperatorProfileRepository constructs the repository.
func NewOperatorProfileRepository(db *sql.DB) *OperatorProfileRepository {
	return &OperatorProfileRepository{db: db}
}

// Get returns the profile for operatorID.
func (r *OperatorProfileRepository) Get(ctx context.Context, operatorID string) (*persistence.OperatorProfile, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator_profile: operator_id required")
	}
	const q = `
SELECT operator_id, structured, notes, created_at, updated_at
FROM operator_profile
WHERE operator_id = ?`
	var p persistence.OperatorProfile
	var createdAt, updatedAt string
	if err := r.db.QueryRowContext(ctx, q, operatorID).Scan(
		&p.OperatorID, &p.Structured, &p.Notes, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("operator_profile: get: %w", err)
	}
	if err := parseOperatorProfileTimes(&p, createdAt, updatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// Upsert creates or replaces the mutable fields of an operator profile.
func (r *OperatorProfileRepository) Upsert(ctx context.Context, p *persistence.OperatorProfile) error {
	if p == nil || p.OperatorID == "" {
		return fmt.Errorf("operator_profile: operator_id required")
	}
	structured := p.Structured
	if len(structured) == 0 {
		structured = []byte("{}")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	const q = `
INSERT INTO operator_profile (operator_id, structured, notes, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(operator_id) DO UPDATE SET
    structured = excluded.structured,
    notes = excluded.notes,
    updated_at = excluded.updated_at`
	if _, err := r.db.ExecContext(ctx, q, p.OperatorID, structured, p.Notes, now, now); err != nil {
		return fmt.Errorf("operator_profile: upsert: %w", err)
	}
	return nil
}

// Delete removes the profile for operatorID.
func (r *OperatorProfileRepository) Delete(ctx context.Context, operatorID string) error {
	if operatorID == "" {
		return fmt.Errorf("operator_profile: operator_id required")
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM operator_profile WHERE operator_id = ?`, operatorID); err != nil {
		return fmt.Errorf("operator_profile: delete: %w", err)
	}
	return nil
}

// List returns the most recently updated profiles, up to limit.
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
LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("operator_profile: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.OperatorProfile
	for rows.Next() {
		var p persistence.OperatorProfile
		var createdAt, updatedAt string
		if err := rows.Scan(&p.OperatorID, &p.Structured, &p.Notes, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("operator_profile: list scan: %w", err)
		}
		if err := parseOperatorProfileTimes(&p, createdAt, updatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operator_profile: list rows: %w", err)
	}
	return out, nil
}

func parseOperatorProfileTimes(p *persistence.OperatorProfile, createdAt, updatedAt string) error {
	var err error
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return fmt.Errorf("operator_profile: parse created_at: %w", err)
	}
	p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return fmt.Errorf("operator_profile: parse updated_at: %w", err)
	}
	return nil
}
