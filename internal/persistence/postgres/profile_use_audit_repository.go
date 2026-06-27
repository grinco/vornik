package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// ProfileUseAuditRepository persists per-turn audit rows for
// `vornikctl operator audit`. Append-only — no Update path.
type ProfileUseAuditRepository struct {
	db DBTX
}

// NewProfileUseAuditRepository constructs the repo over db.
func NewProfileUseAuditRepository(db DBTX) *ProfileUseAuditRepository {
	return &ProfileUseAuditRepository{db: db}
}

// Insert appends one audit row. used_keys is JSON-marshalled
// from the Go slice; a nil slice becomes the JSON literal `[]`
// so the JSONB column's NOT NULL constraint always holds.
func (r *ProfileUseAuditRepository) Insert(ctx context.Context, row *persistence.ProfileUseAudit) error {
	if row == nil || row.OperatorID == "" {
		return fmt.Errorf("profile_use_audit: operator_id required")
	}
	keys := row.UsedKeys
	if keys == nil {
		keys = []string{}
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("profile_use_audit: marshal used_keys: %w", err)
	}
	var taskID interface{}
	if row.TaskID != "" {
		taskID = row.TaskID
	}
	const q = `
INSERT INTO profile_use_audit (operator_id, task_id, used_keys, used_notes)
VALUES ($1, $2, $3, $4)
RETURNING id, created_at`
	if err := r.db.QueryRowContext(ctx, q, row.OperatorID, taskID, keysJSON, row.UsedNotes).Scan(&row.ID, &row.CreatedAt); err != nil {
		return fmt.Errorf("profile_use_audit: insert: %w", err)
	}
	return nil
}

// ListForOperator returns rows newest-first.
func (r *ProfileUseAuditRepository) ListForOperator(ctx context.Context, operatorID string, qy persistence.ProfileUseAuditQuery) ([]*persistence.ProfileUseAudit, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("profile_use_audit: operator_id required")
	}
	limit := qy.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 500:
		limit = 500
	}

	args := []interface{}{operatorID}
	clauses := "WHERE operator_id = $1"
	if !qy.Since.IsZero() {
		args = append(args, qy.Since)
		clauses += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !qy.Until.IsZero() {
		args = append(args, qy.Until)
		clauses += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
SELECT id, operator_id, COALESCE(task_id, ''), used_keys, used_notes, created_at
FROM profile_use_audit
%s
ORDER BY created_at DESC
LIMIT $%d`, clauses, len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("profile_use_audit: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ProfileUseAudit
	for rows.Next() {
		var (
			row      persistence.ProfileUseAudit
			keysJSON []byte
		)
		if err := rows.Scan(&row.ID, &row.OperatorID, &row.TaskID, &keysJSON, &row.UsedNotes, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("profile_use_audit: scan: %w", err)
		}
		if len(keysJSON) > 0 {
			if err := json.Unmarshal(keysJSON, &row.UsedKeys); err != nil {
				return nil, fmt.Errorf("profile_use_audit: unmarshal used_keys: %w", err)
			}
		}
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("profile_use_audit: rows: %w", err)
	}
	return out, nil
}

// DeleteAllForOperator drops every row for one operator.
func (r *ProfileUseAuditRepository) DeleteAllForOperator(ctx context.Context, operatorID string) error {
	if operatorID == "" {
		return fmt.Errorf("profile_use_audit: operator_id required")
	}
	const q = `DELETE FROM profile_use_audit WHERE operator_id = $1`
	if _, err := r.db.ExecContext(ctx, q, operatorID); err != nil {
		return fmt.Errorf("profile_use_audit: delete-all: %w", err)
	}
	return nil
}
