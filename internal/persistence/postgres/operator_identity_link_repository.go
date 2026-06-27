package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// OperatorIdentityLinkRepository implements
// persistence.OperatorIdentityLinkRepository against PostgreSQL.
// Table + index migrated as part of the per-operator profile
// rollout (internal/persistence/migrations.go:3104).
type OperatorIdentityLinkRepository struct {
	db DBTX
}

// NewOperatorIdentityLinkRepository constructs the repo over db.
func NewOperatorIdentityLinkRepository(db DBTX) *OperatorIdentityLinkRepository {
	return &OperatorIdentityLinkRepository{db: db}
}

// Get returns the link row. ErrNotFound when no row exists so
// the dispatcher's resolver can fall through to "use the speaker
// id as the canonical id".
func (r *OperatorIdentityLinkRepository) Get(ctx context.Context, channelSpeakerID string) (*persistence.OperatorIdentityLink, error) {
	if channelSpeakerID == "" {
		return nil, fmt.Errorf("operator_identity_link: channel_speaker_id required")
	}
	const q = `
SELECT channel_speaker_id, operator_id, linked_at, linked_by
FROM operator_identity_link
WHERE channel_speaker_id = $1`
	row := r.db.QueryRowContext(ctx, q, channelSpeakerID)
	var l persistence.OperatorIdentityLink
	if err := row.Scan(&l.ChannelSpeakerID, &l.OperatorID, &l.LinkedAt, &l.LinkedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("operator_identity_link: get: %w", err)
	}
	return &l, nil
}

// ListForOperator returns every speaker that resolves to
// operatorID, ordered by linked_at ASC so the oldest (typically
// the anchor) comes first.
func (r *OperatorIdentityLinkRepository) ListForOperator(ctx context.Context, operatorID string) ([]*persistence.OperatorIdentityLink, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator_identity_link: operator_id required")
	}
	const q = `
SELECT channel_speaker_id, operator_id, linked_at, linked_by
FROM operator_identity_link
WHERE operator_id = $1
ORDER BY linked_at ASC`
	rows, err := r.db.QueryContext(ctx, q, operatorID)
	if err != nil {
		return nil, fmt.Errorf("operator_identity_link: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.OperatorIdentityLink
	for rows.Next() {
		var l persistence.OperatorIdentityLink
		if err := rows.Scan(&l.ChannelSpeakerID, &l.OperatorID, &l.LinkedAt, &l.LinkedBy); err != nil {
			return nil, fmt.Errorf("operator_identity_link: scan: %w", err)
		}
		out = append(out, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operator_identity_link: rows: %w", err)
	}
	return out, nil
}

// Upsert inserts or updates the link row. linked_at stays at its
// existing value on conflict so the audit reflects "when the
// link first formed", not "the last time we touched the row".
func (r *OperatorIdentityLinkRepository) Upsert(ctx context.Context, l *persistence.OperatorIdentityLink) error {
	if l == nil || l.ChannelSpeakerID == "" || l.OperatorID == "" {
		return fmt.Errorf("operator_identity_link: channel_speaker_id and operator_id required")
	}
	linkedBy := l.LinkedBy
	if linkedBy == "" {
		linkedBy = "self"
	}
	const q = `
INSERT INTO operator_identity_link (channel_speaker_id, operator_id, linked_at, linked_by)
VALUES ($1, $2, NOW(), $3)
ON CONFLICT (channel_speaker_id) DO UPDATE
    SET operator_id = EXCLUDED.operator_id,
        linked_by   = EXCLUDED.linked_by`
	if _, err := r.db.ExecContext(ctx, q, l.ChannelSpeakerID, l.OperatorID, linkedBy); err != nil {
		return fmt.Errorf("operator_identity_link: upsert: %w", err)
	}
	return nil
}

// Delete drops one row. Idempotent — missing rows are fine.
func (r *OperatorIdentityLinkRepository) Delete(ctx context.Context, channelSpeakerID string) error {
	if channelSpeakerID == "" {
		return fmt.Errorf("operator_identity_link: channel_speaker_id required")
	}
	const q = `DELETE FROM operator_identity_link WHERE channel_speaker_id = $1`
	if _, err := r.db.ExecContext(ctx, q, channelSpeakerID); err != nil {
		return fmt.Errorf("operator_identity_link: delete: %w", err)
	}
	return nil
}

// DeleteAllForOperator drops every row that resolves to one
// operator. Called by `vornikctl operator forget` so the
// canonical id doesn't outlive the profile.
func (r *OperatorIdentityLinkRepository) DeleteAllForOperator(ctx context.Context, operatorID string) error {
	if operatorID == "" {
		return fmt.Errorf("operator_identity_link: operator_id required")
	}
	const q = `DELETE FROM operator_identity_link WHERE operator_id = $1`
	if _, err := r.db.ExecContext(ctx, q, operatorID); err != nil {
		return fmt.Errorf("operator_identity_link: delete-all: %w", err)
	}
	return nil
}
