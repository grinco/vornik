package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ExecutionLiveEventRepository implements
// persistence.ExecutionLiveEventRepository against PostgreSQL.
//
// Append computes the next per-execution seq via
// `SELECT COALESCE(MAX(seq) + 1, 0)` inside the INSERT. Leader
// election ensures only one replica is running a given execution
// at a time, so concurrent Append on the same execution_id is
// rare — but the (execution_id, seq) unique index is the
// backstop: a duplicate raises an error, the caller retries.
type ExecutionLiveEventRepository struct {
	db DBTX
}

// NewExecutionLiveEventRepository constructs the repo over db.
func NewExecutionLiveEventRepository(db DBTX) *ExecutionLiveEventRepository {
	return &ExecutionLiveEventRepository{db: db}
}

// Append persists one event and returns the assigned seq.
// Payload may be nil — JSONB `null` is then stored.
func (r *ExecutionLiveEventRepository) Append(ctx context.Context, executionID, kind string, payload []byte) (int64, error) {
	if executionID == "" || kind == "" {
		return 0, fmt.Errorf("live_event: execution_id + kind required")
	}
	const q = `
INSERT INTO execution_live_events (execution_id, seq, kind, payload, created_at)
VALUES ($1, COALESCE((SELECT MAX(seq) + 1 FROM execution_live_events WHERE execution_id = $1), 0), $2, $3, NOW())
RETURNING seq`
	// Empty payload becomes JSONB null. Storing []byte(nil) makes
	// pq send the empty bytea, which JSONB rejects.
	var arg interface{} = payload
	if len(payload) == 0 {
		arg = nil
	}
	var seq int64
	if err := r.db.QueryRowContext(ctx, q, executionID, kind, arg).Scan(&seq); err != nil {
		return 0, fmt.Errorf("live_event: append: %w", err)
	}
	return seq, nil
}

// ListSince returns events with seq >= fromSeq, ordered ascending.
// limit caps the result set (0 → no cap; production callers always
// pass a sane bound).
func (r *ExecutionLiveEventRepository) ListSince(ctx context.Context, executionID string, fromSeq int64, limit int) ([]*persistence.ExecutionLiveEvent, error) {
	if executionID == "" {
		return nil, fmt.Errorf("live_event: execution_id required")
	}
	q := `
SELECT id, execution_id, seq, kind, COALESCE(payload::text, ''), created_at
FROM execution_live_events
WHERE execution_id = $1 AND seq >= $2
ORDER BY seq ASC`
	args := []interface{}{executionID, fromSeq}
	if limit > 0 {
		q += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("live_event: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.ExecutionLiveEvent
	for rows.Next() {
		var (
			ev      persistence.ExecutionLiveEvent
			payText string
		)
		if err := rows.Scan(&ev.ID, &ev.ExecutionID, &ev.Seq, &ev.Kind, &payText, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("live_event: list scan: %w", err)
		}
		if payText != "" {
			ev.Payload = []byte(payText)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// LatestSeq returns the largest seq for executionID, or -1 when no
// rows exist.
func (r *ExecutionLiveEventRepository) LatestSeq(ctx context.Context, executionID string) (int64, error) {
	if executionID == "" {
		return -1, fmt.Errorf("live_event: execution_id required")
	}
	const q = `SELECT COALESCE(MAX(seq), -1) FROM execution_live_events WHERE execution_id = $1`
	var seq int64
	if err := r.db.QueryRowContext(ctx, q, executionID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, fmt.Errorf("live_event: latest_seq: %w", err)
	}
	return seq, nil
}

// DeleteOlderThan removes events with created_at older than cutoff.
// Drives the future stale-event sweeper. No cascading effects —
// events on completed executions are pure observability artifacts.
func (r *ExecutionLiveEventRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM execution_live_events WHERE created_at < $1`
	res, err := r.db.ExecContext(ctx, q, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("live_event: prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
