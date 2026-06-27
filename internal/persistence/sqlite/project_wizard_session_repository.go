package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ProjectWizardSessionRepository implements
// persistence.ProjectWizardSessionRepository on SQLite. Same
// schema shape as the postgres mirror; the JSONB columns become
// TEXT and BOOLEAN becomes INTEGER 0/1.
type ProjectWizardSessionRepository struct {
	db *sql.DB
}

// NewProjectWizardSessionRepository wires the repo over a *sql.DB.
func NewProjectWizardSessionRepository(db *sql.DB) *ProjectWizardSessionRepository {
	return &ProjectWizardSessionRepository{db: db}
}

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
	transcript := string(s.Transcript)
	if transcript == "" {
		transcript = "[]"
	}
	readyInt := 0
	if s.ReadyToCommit {
		readyInt = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO project_wizard_sessions (
		    id, created_at, updated_at, operator_id,
		    transcript, current_proposal, suggested_template, ready_to_commit
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, sqliteTime(s.CreatedAt), sqliteTime(s.UpdatedAt), s.OperatorID,
		transcript, nullableSqliteBytes(s.CurrentProposal), nullableSqliteString(s.SuggestedTemplate), readyInt,
	)
	return err
}

func (r *ProjectWizardSessionRepository) Get(ctx context.Context, id string) (*persistence.ProjectWizardSession, error) {
	if id == "" {
		return nil, fmt.Errorf("session ID required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       transcript, current_proposal, suggested_template, ready_to_commit,
		       committed_project_id, committed_at, cancelled_at
		FROM project_wizard_sessions
		WHERE id = ?`, id)
	return scanSqliteWizardSession(row)
}

func (r *ProjectWizardSessionRepository) Update(ctx context.Context, s *persistence.ProjectWizardSession) error {
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
	readyInt := 0
	if s.ReadyToCommit {
		readyInt = 1
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_wizard_sessions
		SET transcript = ?,
		    current_proposal = ?,
		    suggested_template = ?,
		    ready_to_commit = ?,
		    updated_at = ?
		WHERE id = ?`,
		transcript, nullableSqliteBytes(s.CurrentProposal), nullableSqliteString(s.SuggestedTemplate),
		readyInt, sqliteTime(time.Now().UTC()), s.ID,
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

func (r *ProjectWizardSessionRepository) CommitTo(ctx context.Context, sessionID, projectID string) error {
	if sessionID == "" || projectID == "" {
		return fmt.Errorf("session ID and project ID required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_wizard_sessions
		SET committed_project_id = ?,
		    committed_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND committed_project_id IS NULL`,
		projectID, now, now, sessionID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
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
// freeing the operator's active-session slot. The operator_id = ?
// predicate is the IDOR guard. Idempotent — cancelling an
// already-cancelled session is a no-op success. Refuses
// (ErrInvalidTransition) when the row is already committed.
func (r *ProjectWizardSessionRepository) Cancel(ctx context.Context, sessionID, operatorID string) error {
	if sessionID == "" || operatorID == "" {
		return fmt.Errorf("session ID and operator ID required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_wizard_sessions
		SET cancelled_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND operator_id = ?
		  AND committed_project_id IS NULL
		  AND cancelled_at IS NULL`,
		now, now, sessionID, operatorID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		s, getErr := r.Get(ctx, sessionID)
		if getErr != nil {
			return getErr
		}
		if s == nil {
			return persistence.ErrNotFound
		}
		if s.OperatorID != operatorID {
			return persistence.ErrNotFound
		}
		if s.CommittedProjectID != nil {
			return persistence.ErrInvalidTransition
		}
		if s.CancelledAt != nil {
			return nil
		}
		return persistence.ErrNotFound
	}
	return nil
}

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
		WHERE operator_id = ?
		ORDER BY updated_at DESC
		LIMIT ?`, operatorID, pageSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ProjectWizardSession, 0)
	for rows.Next() {
		s, err := scanSqliteWizardSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSqliteWizardSession(scanner interface{ Scan(dest ...any) error }) (*persistence.ProjectWizardSession, error) {
	var (
		s                  persistence.ProjectWizardSession
		createdAt          sqlTime
		updatedAt          sqlTime
		transcript         string
		currentProposal    sql.NullString
		suggestedTemplate  sql.NullString
		readyInt           int
		committedProjectID sql.NullString
		committedAt        sqlNullTime
		cancelledAt        sqlNullTime
	)
	err := scanner.Scan(
		&s.ID, &createdAt, &updatedAt, &s.OperatorID,
		&transcript, &currentProposal, &suggestedTemplate, &readyInt,
		&committedProjectID, &committedAt, &cancelledAt,
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
	if currentProposal.Valid {
		s.CurrentProposal = []byte(currentProposal.String)
	}
	if suggestedTemplate.Valid {
		s.SuggestedTemplate = suggestedTemplate.String
	}
	s.ReadyToCommit = readyInt != 0
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

func nullableSqliteBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func nullableSqliteString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
