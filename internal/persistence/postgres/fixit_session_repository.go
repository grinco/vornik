package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// FixItSessionRepository implements persistence.FixItSessionRepository
// on PostgreSQL. Schema shape mirrors ProjectWizardSessionRepository —
// one row per operator conversation; transcript/last_envelope/
// applied_actions are JSONB blobs the fixitdoctor service round-trips
// opaquely.
type FixItSessionRepository struct {
	db DBTX
}

// NewFixItSessionRepository wires the repo over a *sql.DB.
func NewFixItSessionRepository(db DBTX) *FixItSessionRepository {
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
	transcript := s.Transcript
	if len(transcript) == 0 {
		transcript = []byte("[]")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO fixit_sessions (
		    id, created_at, updated_at, operator_id,
		    failure_kind, failure_ref_id, project_id,
		    transcript, last_envelope, applied_actions, status_signal
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)`,
		s.ID, s.CreatedAt, s.UpdatedAt, s.OperatorID,
		s.FailureKind, s.FailureRefID, nullableStr(s.ProjectID),
		string(transcript), jsonbValue(s.LastEnvelope), jsonbValue(s.AppliedActions), nullableStr(s.StatusSignal),
	)
	return mapDBError(err)
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
		WHERE id = $1`, id)
	return scanFixItSession(row)
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
	transcript := s.Transcript
	if len(transcript) == 0 {
		transcript = []byte("[]")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET transcript       = $2::jsonb,
		    last_envelope    = $3,
		    applied_actions  = $4,
		    status_signal    = $5,
		    updated_at       = NOW()
		WHERE id = $1`,
		s.ID, string(transcript), jsonbValue(s.LastEnvelope), jsonbValue(s.AppliedActions), nullableStr(s.StatusSignal),
	)
	if err != nil {
		return mapDBError(err)
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
		WHERE operator_id = $1
		ORDER BY updated_at DESC
		LIMIT $2`, operatorID, pageSize)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.FixItSession, 0)
	for rows.Next() {
		s, err := scanFixItSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Close stamps closed_at on a session owned by operatorID. The
// operator_id = $2 predicate is the IDOR guard: a session belonging to
// another operator is invisible to the caller. Idempotent — closing an
// already-closed session is a no-op success.
func (r *FixItSessionRepository) Close(ctx context.Context, id, operatorID string) error {
	if id == "" || operatorID == "" {
		return fmt.Errorf("session ID and operator ID required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET closed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND operator_id = $2
		  AND closed_at IS NULL`,
		id, operatorID,
	)
	if err != nil {
		return mapDBError(err)
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
	res, err := r.db.ExecContext(ctx, `
		UPDATE fixit_sessions
		SET closed_at = NOW(),
		    updated_at = NOW()
		WHERE failure_kind = $1
		  AND failure_ref_id = $2
		  AND closed_at IS NULL`,
		failureKind, failureRefID,
	)
	if err != nil {
		return 0, mapDBError(err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanFixItSession(scanner interface {
	Scan(dest ...any) error
}) (*persistence.FixItSession, error) {
	var (
		s              persistence.FixItSession
		transcriptStr  string
		projectID      sql.NullString
		lastEnvelope   sql.NullString
		appliedActions sql.NullString
		statusSignal   sql.NullString
		closedAt       sql.NullTime
	)
	err := scanner.Scan(
		&s.ID, &s.CreatedAt, &s.UpdatedAt, &s.OperatorID,
		&s.FailureKind, &s.FailureRefID, &projectID,
		&transcriptStr, &lastEnvelope, &appliedActions, &statusSignal, &closedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	s.Transcript = []byte(transcriptStr)
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
