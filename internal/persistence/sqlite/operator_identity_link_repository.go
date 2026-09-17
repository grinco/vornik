package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// OperatorIdentityLinkRepository persists cross-channel speaker→operator
// links on SQLite (operator-profile-design Phase A).
//
// It was a no-op stub until 2026-09-15: Upsert returned nil and stored
// nothing, Get returned ErrNotFound, and the dispatcher's resolver read that
// as "this speaker is its own canonical id". The stated reason was that
// single-process deployments do not need cross-process persistence, which
// conflates two things — a link has to survive a RESTART, and nothing about
// running one process makes that unnecessary.
//
// What it actually broke, on every SQLite deployment: `/link` between two
// chats reported a successful consolidation and consolidated nothing, and the
// Phase-4 link-code redemption repointed a profile onto the account and then
// audited profile_repoint:"merged" having moved nothing. Both are the failure
// the operator_profile table beside it already carries a warning about —
// acknowledging a write you discard creates false memory.
type OperatorIdentityLinkRepository struct {
	db *sql.DB
}

// NewOperatorIdentityLinkRepository constructs the repository.
func NewOperatorIdentityLinkRepository(db *sql.DB) *OperatorIdentityLinkRepository {
	return &OperatorIdentityLinkRepository{db: db}
}

// Get returns the link row for one channel speaker id, or ErrNotFound when
// the speaker is not linked — which callers read as "the speaker id is its
// own canonical id".
func (r *OperatorIdentityLinkRepository) Get(ctx context.Context, channelSpeakerID string) (*persistence.OperatorIdentityLink, error) {
	if channelSpeakerID == "" {
		return nil, fmt.Errorf("operator_identity_link: channel_speaker_id required")
	}
	const q = `
SELECT channel_speaker_id, operator_id, linked_by, linked_at
FROM operator_identity_link
WHERE channel_speaker_id = ?`
	var row persistence.OperatorIdentityLink
	var linkedAt string
	if err := r.db.QueryRowContext(ctx, q, channelSpeakerID).Scan(
		&row.ChannelSpeakerID, &row.OperatorID, &row.LinkedBy, &linkedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, fmt.Errorf("operator_identity_link: get: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, linkedAt)
	if err != nil {
		return nil, fmt.Errorf("operator_identity_link: parse linked_at: %w", err)
	}
	row.LinkedAt = t
	return &row, nil
}

// ListForOperator returns every speaker id resolving to operatorID. The merge
// path uses it to repoint a loser's rows, so a miss is an empty slice rather
// than an error.
func (r *OperatorIdentityLinkRepository) ListForOperator(ctx context.Context, operatorID string) ([]*persistence.OperatorIdentityLink, error) {
	const q = `
SELECT channel_speaker_id, operator_id, linked_by, linked_at
FROM operator_identity_link
WHERE operator_id = ?
ORDER BY linked_at ASC`
	rows, err := r.db.QueryContext(ctx, q, operatorID)
	if err != nil {
		return nil, fmt.Errorf("operator_identity_link: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.OperatorIdentityLink
	for rows.Next() {
		var row persistence.OperatorIdentityLink
		var linkedAt string
		if err := rows.Scan(&row.ChannelSpeakerID, &row.OperatorID, &row.LinkedBy, &linkedAt); err != nil {
			return nil, fmt.Errorf("operator_identity_link: scan: %w", err)
		}
		t, terr := time.Parse(time.RFC3339Nano, linkedAt)
		if terr != nil {
			return nil, fmt.Errorf("operator_identity_link: parse linked_at: %w", terr)
		}
		row.LinkedAt = t
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operator_identity_link: rows: %w", err)
	}
	return out, nil
}

// Upsert inserts or repoints a link row.
//
// linked_at is stamped by the REPOSITORY on first insert and untouched on
// repoint, exactly as the interface contract says and exactly as the Postgres
// twin does (which writes NOW() and never reads the caller's value). The
// caller's LinkedAt is deliberately ignored: this first cut honoured it, which
// let the two drivers disagree about a column an auditor reads — SQLite would
// take whatever a caller passed, Postgres would stamp its own clock. A
// repository that records when something happened should not accept the
// answer from the thing it is recording.
//
// On repoint the original survives, because the column records when the
// operator FIRST confirmed the relationship, not when a merge last rewrote
// the row.
func (r *OperatorIdentityLinkRepository) Upsert(ctx context.Context, link *persistence.OperatorIdentityLink) error {
	if link == nil || link.ChannelSpeakerID == "" || link.OperatorID == "" {
		return fmt.Errorf("operator_identity_link: channel_speaker_id and operator_id required")
	}
	linkedBy := link.LinkedBy
	if linkedBy == "" {
		linkedBy = "self"
	}
	const q = `
INSERT INTO operator_identity_link (channel_speaker_id, operator_id, linked_by, linked_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (channel_speaker_id)
DO UPDATE SET operator_id = excluded.operator_id, linked_by = excluded.linked_by`
	if _, err := r.db.ExecContext(ctx, q,
		link.ChannelSpeakerID, link.OperatorID, linkedBy,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("operator_identity_link: upsert: %w", err)
	}
	return nil
}

// Delete removes one link row. Idempotent: a missing row is not an error,
// matching the interface contract.
func (r *OperatorIdentityLinkRepository) Delete(ctx context.Context, channelSpeakerID string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM operator_identity_link WHERE channel_speaker_id = ?`, channelSpeakerID); err != nil {
		return fmt.Errorf("operator_identity_link: delete: %w", err)
	}
	return nil
}

// DeleteAllForOperator removes every link row pointing at operatorID.
func (r *OperatorIdentityLinkRepository) DeleteAllForOperator(ctx context.Context, operatorID string) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM operator_identity_link WHERE operator_id = ?`, operatorID); err != nil {
		return fmt.Errorf("operator_identity_link: delete all: %w", err)
	}
	return nil
}
