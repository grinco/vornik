package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProjectWizardSessionRepository implements
// persistence.ProjectWizardSessionRepository on PostgreSQL.
//
// One row per operator conversation; transcript + current_proposal
// are JSONB blobs the wizard service round-trips opaquely. The
// repo doesn't decode them — kind/role-specific logic lives in
// internal/projectwizard.
type ProjectWizardSessionRepository struct {
	db DBTX
}

// NewProjectWizardSessionRepository wires the repo over a *sql.DB.
func NewProjectWizardSessionRepository(db DBTX) *ProjectWizardSessionRepository {
	return &ProjectWizardSessionRepository{db: db}
}

// Insert creates a fresh session row. Caller is responsible for
// ID generation via persistence.GenerateID("pw").
func (r *ProjectWizardSessionRepository) Insert(ctx context.Context, s *persistence.ProjectWizardSession) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.ID == "" {
		return fmt.Errorf("session ID required")
	}
	if s.OperatorID == "" {
		return fmt.Errorf("operator ID required")
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
		INSERT INTO project_wizard_sessions (
		    id, created_at, updated_at, operator_id,
		    transcript, current_proposal, suggested_template, ready_to_commit
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)`,
		s.ID, s.CreatedAt, s.UpdatedAt, s.OperatorID,
		string(transcript), jsonbValue(s.CurrentProposal), nullableStr(s.SuggestedTemplate), s.ReadyToCommit,
	)
	return mapDBError(err)
}

// Get fetches a session by ID. ErrNotFound when missing.
func (r *ProjectWizardSessionRepository) Get(ctx context.Context, id string) (*persistence.ProjectWizardSession, error) {
	if id == "" {
		return nil, fmt.Errorf("session ID required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       transcript, current_proposal, suggested_template, ready_to_commit,
		       committed_project_id, committed_at, cancelled_at
		FROM project_wizard_sessions
		WHERE id = $1`, id)
	return scanProjectWizardSession(row)
}

// Update rewrites mutable columns. updated_at bumped server-side.
// Caller leaves committed_project_id / committed_at unchanged; use
// CommitTo for the terminal flip.
func (r *ProjectWizardSessionRepository) Update(ctx context.Context, s *persistence.ProjectWizardSession) error {
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
		UPDATE project_wizard_sessions
		SET transcript        = $2::jsonb,
		    current_proposal  = $3,
		    suggested_template = $4,
		    ready_to_commit   = $5,
		    updated_at        = NOW()
		WHERE id = $1`,
		s.ID, string(transcript), jsonbValue(s.CurrentProposal), nullableStr(s.SuggestedTemplate), s.ReadyToCommit,
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

// CommitTo atomically stamps committed_project_id + committed_at.
// Refuses (ErrInvalidTransition) when the row is already committed
// so a double-click on the UI commit button is safe.
func (r *ProjectWizardSessionRepository) CommitTo(ctx context.Context, sessionID, projectID string) error {
	if sessionID == "" || projectID == "" {
		return fmt.Errorf("session ID and project ID required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_wizard_sessions
		SET committed_project_id = $2,
		    committed_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND committed_project_id IS NULL`,
		sessionID, projectID,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either missing or already committed — disambiguate with
		// a follow-up read.
		s, getErr := r.Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		if s != nil && s.CommittedProjectID != nil {
			return persistence.ErrInvalidTransition
		}
		return persistence.ErrNotFound
	}
	return nil
}

// Cancel atomically stamps cancelled_at on an uncommitted session,
// freeing the operator's active-session slot. The operator_id = $2
// predicate is the IDOR guard: a session belonging to another
// operator is invisible to the caller. Idempotent — cancelling an
// already-cancelled session is a no-op success. Refuses
// (ErrInvalidTransition) when the row is already committed.
func (r *ProjectWizardSessionRepository) Cancel(ctx context.Context, sessionID, operatorID string) error {
	if sessionID == "" || operatorID == "" {
		return fmt.Errorf("session ID and operator ID required")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_wizard_sessions
		SET cancelled_at = NOW(),
		    updated_at = NOW()
		WHERE id = $1
		  AND operator_id = $2
		  AND committed_project_id IS NULL
		  AND cancelled_at IS NULL`,
		sessionID, operatorID,
	)
	if err != nil {
		return mapDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Missing, already committed, already cancelled, or owned by a
		// different operator — disambiguate with a follow-up read.
		s, getErr := r.Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		if s == nil {
			return persistence.ErrNotFound
		}
		// Don't leak existence to the wrong operator.
		if s.OperatorID != operatorID {
			return persistence.ErrNotFound
		}
		if s.CommittedProjectID != nil {
			return persistence.ErrInvalidTransition
		}
		if s.CancelledAt != nil {
			// Idempotent: already cancelled is a success.
			return nil
		}
		return persistence.ErrNotFound
	}
	return nil
}

// ListByOperator returns the operator's most recently-updated
// sessions, capped at pageSize.
func (r *ProjectWizardSessionRepository) ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.ProjectWizardSession, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator ID required")
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       transcript, current_proposal, suggested_template, ready_to_commit,
		       committed_project_id, committed_at, cancelled_at
		FROM project_wizard_sessions
		WHERE operator_id = $1
		ORDER BY updated_at DESC
		LIMIT $2`, operatorID, pageSize)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ProjectWizardSession, 0)
	for rows.Next() {
		s, err := scanProjectWizardSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanProjectWizardSession(scanner interface {
	Scan(dest ...any) error
}) (*persistence.ProjectWizardSession, error) {
	var (
		s                  persistence.ProjectWizardSession
		transcriptStr      string
		currentProposal    sql.NullString
		suggestedTemplate  sql.NullString
		committedProjectID sql.NullString
		committedAt        sql.NullTime
		cancelledAt        sql.NullTime
	)
	err := scanner.Scan(
		&s.ID, &s.CreatedAt, &s.UpdatedAt, &s.OperatorID,
		&transcriptStr, &currentProposal, &suggestedTemplate, &s.ReadyToCommit,
		&committedProjectID, &committedAt, &cancelledAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	s.Transcript = []byte(transcriptStr)
	if currentProposal.Valid {
		s.CurrentProposal = []byte(currentProposal.String)
	}
	if suggestedTemplate.Valid {
		s.SuggestedTemplate = suggestedTemplate.String
	}
	if committedProjectID.Valid {
		s.CommittedProjectID = &committedProjectID.String
	}
	if committedAt.Valid {
		t := committedAt.Time
		s.CommittedAt = &t
	}
	if cancelledAt.Valid {
		t := cancelledAt.Time
		s.CancelledAt = &t
	}
	return &s, nil
}

// nullableStr converts a possibly-empty string to a nullable
// driver value. Distinct from the existing nullableString helper
// (which takes *string) so we don't pollute the package surface.
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
