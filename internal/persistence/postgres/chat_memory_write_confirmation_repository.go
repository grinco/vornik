package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 (part 2) of the chat memory-write design §5.3 (revision 8). Postgres implementations
// of the two repositories behind the shared-scope confirmation state machine. Migration 146
// created both tables; nothing read them until this slice.
//
// The dialect diverges from the SQLite side (TIMESTAMPTZ vs RFC3339 TEXT, ON CONFLICT DO
// UPDATE, RETURNING), which is exactly why the shared repotest contract suite runs on BOTH
// backends — `go test ./...` is sqlite-only and would not catch a Postgres-side break.

// ChatMemoryWriteConfirmationRepository is the Postgres implementation of the transient
// pending-confirmation store (migration 146).
type ChatMemoryWriteConfirmationRepository struct {
	db persistence.DBTX
}

// NewChatMemoryWriteConfirmationRepository builds the repository.
func NewChatMemoryWriteConfirmationRepository(db persistence.DBTX) *ChatMemoryWriteConfirmationRepository {
	return &ChatMemoryWriteConfirmationRepository{db: db}
}

var _ persistence.ChatMemoryWriteConfirmationRepository = (*ChatMemoryWriteConfirmationRepository)(nil)

// Propose creates or REPLACES the pending confirmation for (channel, session_id), clearing any
// previous acknowledgement. The upsert is what makes a superseded proposal unacknowledgeable
// rather than dormant (§5.3.1): a second proposal overwrites the first, so an acknowledgement
// can only ever discharge the most recent one.
func (r *ChatMemoryWriteConfirmationRepository) Propose(ctx context.Context, c *persistence.ChatMemoryWriteConfirmation) error {
	if c == nil {
		return errors.New("postgres: chat_memory_write_confirmations propose: nil confirmation")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_memory_write_confirmations
		     (channel, session_id, content_fingerprint, scope, operator_id, proposed_at, expires_at, acknowledged_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)
		 ON CONFLICT (channel, session_id) DO UPDATE SET
		     content_fingerprint = EXCLUDED.content_fingerprint,
		     scope               = EXCLUDED.scope,
		     operator_id         = EXCLUDED.operator_id,
		     proposed_at         = EXCLUDED.proposed_at,
		     expires_at          = EXCLUDED.expires_at,
		     acknowledged_at     = NULL`,
		c.Channel, c.SessionID, c.ContentFingerprint, c.Scope, c.OperatorID,
		c.ProposedAt.UTC(), c.ExpiresAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("postgres: chat_memory_write_confirmations upsert: %w", err)
	}
	return nil
}

// Get returns the pending confirmation, or nil when there is none.
func (r *ChatMemoryWriteConfirmationRepository) Get(ctx context.Context, channel, sessionID string) (*persistence.ChatMemoryWriteConfirmation, error) {
	var (
		c   persistence.ChatMemoryWriteConfirmation
		ack sql.NullTime
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT channel, session_id, content_fingerprint, scope, operator_id,
		        proposed_at, expires_at, acknowledged_at
		   FROM chat_memory_write_confirmations
		  WHERE channel = $1 AND session_id = $2`,
		channel, sessionID,
	).Scan(&c.Channel, &c.SessionID, &c.ContentFingerprint, &c.Scope, &c.OperatorID,
		&c.ProposedAt, &c.ExpiresAt, &ack)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: chat_memory_write_confirmations get: %w", err)
	}
	if ack.Valid {
		t := ack.Time
		c.AcknowledgedAt = &t
	}
	return &c, nil
}

// Acknowledge stamps acknowledged_at, but ONLY when operator_id matches — the same-speaker
// binding lives in the storage layer as well as in dispatcher.authorizeSharedWrite so a caller
// that forgets to pass the operator cannot widen the grant. Reports whether a row was stamped.
func (r *ChatMemoryWriteConfirmationRepository) Acknowledge(ctx context.Context, channel, sessionID, operatorID string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_memory_write_confirmations
		    SET acknowledged_at = $4
		  WHERE channel = $1 AND session_id = $2 AND operator_id = $3`,
		channel, sessionID, operatorID, at.UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("postgres: chat_memory_write_confirmations acknowledge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("postgres: chat_memory_write_confirmations acknowledge rows: %w", err)
	}
	return n > 0, nil
}

// Delete removes the pending confirmation. Called after a granted decision (one-shot) and when
// a row is found expired.
func (r *ChatMemoryWriteConfirmationRepository) Delete(ctx context.Context, channel, sessionID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_memory_write_confirmations WHERE channel = $1 AND session_id = $2`,
		channel, sessionID,
	)
	if err != nil {
		return fmt.Errorf("postgres: chat_memory_write_confirmations delete: %w", err)
	}
	return nil
}

// DeleteExpired sweeps rows at or past expires_at so a crashed conversation cannot leave a
// pending confirmation indefinitely (§5.3.3). Returns the number removed.
func (r *ChatMemoryWriteConfirmationRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_memory_write_confirmations WHERE expires_at <= $1`,
		now.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("postgres: chat_memory_write_confirmations delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("postgres: chat_memory_write_confirmations delete expired rows: %w", err)
	}
	return n, nil
}

// ChatMemoryWriteAuditRepository is the Postgres implementation of the append-only
// shared-write attestation store (migration 146).
type ChatMemoryWriteAuditRepository struct {
	db persistence.DBTX
}

// NewChatMemoryWriteAuditRepository builds the repository.
func NewChatMemoryWriteAuditRepository(db persistence.DBTX) *ChatMemoryWriteAuditRepository {
	return &ChatMemoryWriteAuditRepository{db: db}
}

var _ persistence.ChatMemoryWriteAuditRepository = (*ChatMemoryWriteAuditRepository)(nil)

// Record appends one attestation. Append-only: no ON CONFLICT, no update path.
func (r *ChatMemoryWriteAuditRepository) Record(ctx context.Context, a *persistence.ChatMemoryWriteAudit) error {
	if a == nil {
		return errors.New("postgres: chat_memory_write_audit record: nil audit")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_memory_write_audit
		     (channel, session_id, content_fingerprint, scope, operator_id,
		      proposed_at, acknowledged_at, granted_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.Channel, a.SessionID, a.ContentFingerprint, a.Scope, a.OperatorID,
		a.ProposedAt.UTC(), a.AcknowledgedAt.UTC(), a.GrantedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("postgres: chat_memory_write_audit insert: %w", err)
	}
	return nil
}

// ListByFingerprint returns the attestations for a content fingerprint, oldest first.
func (r *ChatMemoryWriteAuditRepository) ListByFingerprint(ctx context.Context, fingerprint string) ([]persistence.ChatMemoryWriteAudit, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT channel, session_id, content_fingerprint, scope, operator_id,
		        proposed_at, acknowledged_at, granted_at
		   FROM chat_memory_write_audit
		  WHERE content_fingerprint = $1
		  ORDER BY granted_at, id`,
		fingerprint,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: chat_memory_write_audit select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.ChatMemoryWriteAudit
	for rows.Next() {
		var a persistence.ChatMemoryWriteAudit
		if err := rows.Scan(&a.Channel, &a.SessionID, &a.ContentFingerprint, &a.Scope, &a.OperatorID,
			&a.ProposedAt, &a.AcknowledgedAt, &a.GrantedAt); err != nil {
			return nil, fmt.Errorf("postgres: chat_memory_write_audit scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: chat_memory_write_audit rows: %w", err)
	}
	return out, nil
}
