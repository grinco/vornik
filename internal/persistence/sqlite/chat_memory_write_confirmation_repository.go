package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 (part 2) of the chat memory-write design §5.3 (revision 8). SQLite implementations
// of the two repositories behind the shared-scope confirmation state machine.
//
// Real implementations, not the no-op stub some single-node repos use: a shared-scope write
// cannot be authorized without a durable acknowledgement, so a stub here would make the whole
// feature dark on sqlite deployments. TIMESTAMPTZ drops to RFC3339Nano TEXT (via sqliteTime),
// which keeps BEFORE/<= comparisons correct because every value is UTC in one format. The
// shared repotest contract suite runs against BOTH backends to catch a dialect divergence.

// ChatMemoryWriteConfirmationRepository is the SQLite implementation of the transient
// pending-confirmation store.
type ChatMemoryWriteConfirmationRepository struct {
	db *sql.DB
}

// NewChatMemoryWriteConfirmationRepository builds the repository.
func NewChatMemoryWriteConfirmationRepository(db *sql.DB) *ChatMemoryWriteConfirmationRepository {
	return &ChatMemoryWriteConfirmationRepository{db: db}
}

var _ persistence.ChatMemoryWriteConfirmationRepository = (*ChatMemoryWriteConfirmationRepository)(nil)

// Propose creates or REPLACES the pending confirmation, clearing any previous acknowledgement
// (§5.3.1).
func (r *ChatMemoryWriteConfirmationRepository) Propose(ctx context.Context, c *persistence.ChatMemoryWriteConfirmation) error {
	if c == nil {
		return errors.New("sqlite: chat_memory_write_confirmations propose: nil confirmation")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_memory_write_confirmations
		     (channel, session_id, content_fingerprint, scope, operator_id, proposed_at, expires_at, acknowledged_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		 ON CONFLICT (channel, session_id) DO UPDATE SET
		     content_fingerprint = excluded.content_fingerprint,
		     scope               = excluded.scope,
		     operator_id         = excluded.operator_id,
		     proposed_at         = excluded.proposed_at,
		     expires_at          = excluded.expires_at,
		     acknowledged_at     = NULL`,
		c.Channel, c.SessionID, c.ContentFingerprint, c.Scope, c.OperatorID,
		sqliteTime(c.ProposedAt), sqliteTime(c.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("sqlite: chat_memory_write_confirmations upsert: %w", err)
	}
	return nil
}

// Get returns the pending confirmation, or nil when there is none.
func (r *ChatMemoryWriteConfirmationRepository) Get(ctx context.Context, channel, sessionID string) (*persistence.ChatMemoryWriteConfirmation, error) {
	var (
		c          persistence.ChatMemoryWriteConfirmation
		proposedAt sqlTime
		expiresAt  sqlTime
		ack        sqlNullTime
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT channel, session_id, content_fingerprint, scope, operator_id,
		        proposed_at, expires_at, acknowledged_at
		   FROM chat_memory_write_confirmations
		  WHERE channel = ? AND session_id = ?`,
		channel, sessionID,
	).Scan(&c.Channel, &c.SessionID, &c.ContentFingerprint, &c.Scope, &c.OperatorID,
		&proposedAt, &expiresAt, &ack)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: chat_memory_write_confirmations get: %w", err)
	}
	c.ProposedAt = proposedAt.Time
	c.ExpiresAt = expiresAt.Time
	if ack.Valid {
		t := ack.Time
		c.AcknowledgedAt = &t
	}
	return &c, nil
}

// Acknowledge stamps acknowledged_at, but ONLY when operator_id matches. Reports whether a row
// was stamped.
func (r *ChatMemoryWriteConfirmationRepository) Acknowledge(ctx context.Context, channel, sessionID, operatorID string, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_memory_write_confirmations
		    SET acknowledged_at = ?
		  WHERE channel = ? AND session_id = ? AND operator_id = ?`,
		sqliteTime(at), channel, sessionID, operatorID,
	)
	if err != nil {
		return false, fmt.Errorf("sqlite: chat_memory_write_confirmations acknowledge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: chat_memory_write_confirmations acknowledge rows: %w", err)
	}
	return n > 0, nil
}

// Delete removes the pending confirmation (one-shot on grant, and on an expired-row find).
func (r *ChatMemoryWriteConfirmationRepository) Delete(ctx context.Context, channel, sessionID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_memory_write_confirmations WHERE channel = ? AND session_id = ?`,
		channel, sessionID,
	)
	if err != nil {
		return fmt.Errorf("sqlite: chat_memory_write_confirmations delete: %w", err)
	}
	return nil
}

// DeleteExpired sweeps rows at or past expires_at. String comparison is correct because every
// timestamp is written as UTC RFC3339Nano, which is lexicographically ordered.
func (r *ChatMemoryWriteConfirmationRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_memory_write_confirmations WHERE expires_at <= ?`,
		sqliteTime(now),
	)
	if err != nil {
		return 0, fmt.Errorf("sqlite: chat_memory_write_confirmations delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: chat_memory_write_confirmations delete expired rows: %w", err)
	}
	return n, nil
}

// ChatMemoryWriteAuditRepository is the SQLite implementation of the append-only shared-write
// attestation store.
type ChatMemoryWriteAuditRepository struct {
	db *sql.DB
}

// NewChatMemoryWriteAuditRepository builds the repository.
func NewChatMemoryWriteAuditRepository(db *sql.DB) *ChatMemoryWriteAuditRepository {
	return &ChatMemoryWriteAuditRepository{db: db}
}

var _ persistence.ChatMemoryWriteAuditRepository = (*ChatMemoryWriteAuditRepository)(nil)

// Record appends one attestation. Append-only: no ON CONFLICT, no update path.
func (r *ChatMemoryWriteAuditRepository) Record(ctx context.Context, a *persistence.ChatMemoryWriteAudit) error {
	if a == nil {
		return errors.New("sqlite: chat_memory_write_audit record: nil audit")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_memory_write_audit
		     (channel, session_id, content_fingerprint, scope, operator_id,
		      proposed_at, acknowledged_at, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Channel, a.SessionID, a.ContentFingerprint, a.Scope, a.OperatorID,
		sqliteTime(a.ProposedAt), sqliteTime(a.AcknowledgedAt), sqliteTime(a.GrantedAt),
	)
	if err != nil {
		return fmt.Errorf("sqlite: chat_memory_write_audit insert: %w", err)
	}
	return nil
}

// ListByFingerprint returns the attestations for a content fingerprint, oldest first.
func (r *ChatMemoryWriteAuditRepository) ListByFingerprint(ctx context.Context, fingerprint string) ([]persistence.ChatMemoryWriteAudit, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT channel, session_id, content_fingerprint, scope, operator_id,
		        proposed_at, acknowledged_at, granted_at
		   FROM chat_memory_write_audit
		  WHERE content_fingerprint = ?
		  ORDER BY granted_at, id`,
		fingerprint,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: chat_memory_write_audit select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []persistence.ChatMemoryWriteAudit
	for rows.Next() {
		var (
			a          persistence.ChatMemoryWriteAudit
			proposedAt sqlTime
			ackAt      sqlTime
			grantedAt  sqlTime
		)
		if err := rows.Scan(&a.Channel, &a.SessionID, &a.ContentFingerprint, &a.Scope, &a.OperatorID,
			&proposedAt, &ackAt, &grantedAt); err != nil {
			return nil, fmt.Errorf("sqlite: chat_memory_write_audit scan: %w", err)
		}
		a.ProposedAt = proposedAt.Time
		a.AcknowledgedAt = ackAt.Time
		a.GrantedAt = grantedAt.Time
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: chat_memory_write_audit rows: %w", err)
	}
	return out, nil
}
