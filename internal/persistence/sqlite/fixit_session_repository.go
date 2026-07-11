package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// FixItSessionRepository implements persistence.FixItSessionRepository
// on SQLite. Same schema shape as the postgres mirror; the JSONB
// columns become TEXT.
type FixItSessionRepository struct {
	db *sql.DB
}

// NewFixItSessionRepository wires the repo over a *sql.DB.
func NewFixItSessionRepository(db *sql.DB) *FixItSessionRepository {
	return &FixItSessionRepository{db: db}
}

// Insert creates a fresh session row. Caller is responsible for ID
// generation via persistence.GenerateID("fix").
func (r *FixItSessionRepository) Insert(ctx context.Context, s *persistence.FixItSession) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.ID == "" {
		return fmt.Errorf("session ID required")
	}
	if s.OperatorID == "" {
		return fmt.Errorf("operator ID required")
	}
	if s.FailureKind == "" || s.FailureRefID == "" {
		return fmt.Errorf("failure kind and ref id required")
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}
	transcript := string(s.Transcript)
	if transcript == "" {
		transcript = "[]"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fixit_sessions (
		    id, created_at, updated_at, operator_id,
		    failure_kind, failure_ref_id, project_id,
		    transcript, last_envelope, applied_actions, status_signal
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, sqliteTime(s.CreatedAt), sqliteTime(s.UpdatedAt), s.OperatorID,
		s.FailureKind, s.FailureRefID, nullableSqliteString(s.ProjectID),
		transcript, nullableSqliteBytes(s.LastEnvelope), nullableSqliteBytes(s.AppliedActions), nullableSqliteString(s.StatusSignal),
	)
	return err
}

// Get fetches a session by ID. ErrNotFound when missing.
func (r *FixItSessionRepository) Get(ctx context.Context, id string) (*persistence.FixItSession, error) {
	if id == "" {
		return nil, fmt.Errorf("session ID required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       failure_kind, failure_ref_id, project_id,
		       transcript, last_envelope, applied_actions, status_signal, closed_at
		FROM fixit_sessions
		WHERE id = ?`, id)
	return scanSqliteFixItSession(row)
}

// Update rewrites mutable columns. updated_at bumped server-side.
// Caller leaves closed_at unchanged; use Close /
// CascadeCloseByFailureRef for the terminal flip.
func (r *FixItSessionRepository) Update(ctx context.Context, s *persistence.FixItSession) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.ID == "" {
		return fmt.Errorf("session ID required")
	}
	transcript := string(s.Transcript)
	if transcript == "" {
		transcript = "[]"
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET transcript      = ?,
		    last_envelope   = ?,
		    applied_actions = ?,
		    status_signal   = ?,
		    updated_at      = ?
		WHERE id = ?`,
		transcript, nullableSqliteBytes(s.LastEnvelope), nullableSqliteBytes(s.AppliedActions),
		nullableSqliteString(s.StatusSignal), sqliteTime(time.Now().UTC()), s.ID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByOperator returns the operator's most recently-updated
// sessions, capped at pageSize.
func (r *FixItSessionRepository) ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.FixItSession, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator ID required")
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       failure_kind, failure_ref_id, project_id,
		       transcript, last_envelope, applied_actions, status_signal, closed_at
		FROM fixit_sessions
		WHERE operator_id = ?
		ORDER BY updated_at DESC
		LIMIT ?`, operatorID, pageSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.FixItSession, 0)
	for rows.Next() {
		s, err := scanSqliteFixItSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Close stamps closed_at on a session owned by operatorID. The
// operator_id = ? predicate is the IDOR guard: a session belonging to
// another operator is invisible to the caller. Idempotent — closing an
// already-closed session is a no-op success.
func (r *FixItSessionRepository) Close(ctx context.Context, id, operatorID string) error {
	if id == "" || operatorID == "" {
		return fmt.Errorf("session ID and operator ID required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET closed_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND operator_id = ?
		  AND closed_at IS NULL`,
		now, now, id, operatorID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		s, getErr := r.Get(ctx, id)
		if getErr != nil {
			return getErr
		}
		if s == nil {
			return persistence.ErrNotFound
		}
		if s.OperatorID != operatorID {
			return persistence.ErrNotFound
		}
		if s.ClosedAt != nil {
			return nil // idempotent
		}
		return persistence.ErrNotFound
	}
	return nil
}

// CascadeCloseByFailureRef closes every open session (any operator)
// bound to the given failure kind + ref id.
func (r *FixItSessionRepository) CascadeCloseByFailureRef(ctx context.Context, failureKind, failureRefID string) (int, error) {
	if failureKind == "" || failureRefID == "" {
		return 0, fmt.Errorf("failure kind and ref id required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET closed_at = ?,
		    updated_at = ?
		WHERE failure_kind = ?
		  AND failure_ref_id = ?
		  AND closed_at IS NULL`,
		now, now, failureKind, failureRefID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanSqliteFixItSession(scanner interface{ Scan(dest ...any) error }) (*persistence.FixItSession, error) {
	var (
		s              persistence.FixItSession
		createdAt      sqlTime
		updatedAt      sqlTime
		transcript     string
		projectID      sql.NullString
		lastEnvelope   sql.NullString
		appliedActions sql.NullString
		statusSignal   sql.NullString
		closedAt       sqlNullTime
	)
	err := scanner.Scan(
		&s.ID, &createdAt, &updatedAt, &s.OperatorID,
		&s.FailureKind, &s.FailureRefID, &projectID,
		&transcript, &lastEnvelope, &appliedActions, &statusSignal, &closedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	s.CreatedAt = createdAt.Time
	s.UpdatedAt = updatedAt.Time
	s.Transcript = []byte(transcript)
	if projectID.Valid {
		s.ProjectID = projectID.String
	}
	if lastEnvelope.Valid {
		s.LastEnvelope = []byte(lastEnvelope.String)
	}
	if appliedActions.Valid {
		s.AppliedActions = []byte(appliedActions.String)
	}
	if statusSignal.Valid {
		s.StatusSignal = statusSignal.String
	}
	if closedAt.Valid {
		t := closedAt.Time
		s.ClosedAt = &t
	}
	return &s, nil
}
