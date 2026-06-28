package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// InstallationOnboardingSessionRepository implements the durable
// installation-scoped onboarding session repository on SQLite.
type InstallationOnboardingSessionRepository struct {
	db *sql.DB
}

// NewInstallationOnboardingSessionRepository wires the repo over a
// *sql.DB.
func NewInstallationOnboardingSessionRepository(db *sql.DB) *InstallationOnboardingSessionRepository {
	return &InstallationOnboardingSessionRepository{db: db}
}

// Insert persists a new onboarding session. Returns an error if the
// session is nil or missing required fields.
func (r *InstallationOnboardingSessionRepository) Insert(ctx context.Context, s *persistence.InstallationOnboardingSession) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.ID == "" {
		return fmt.Errorf("session ID required")
	}
	if s.OperatorID == "" {
		return fmt.Errorf("operator ID required")
	}
	if s.CurrentStep == "" {
		return fmt.Errorf("current step required")
	}
	if s.SelectedUseCase == "" {
		return fmt.Errorf("selected use case required")
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
		INSERT INTO installation_onboarding_sessions (
		    id, created_at, updated_at, operator_id,
		    current_step, selected_use_case, transcript,
		    proposed_config, proposed_project, validation_results
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, sqliteTime(s.CreatedAt), sqliteTime(s.UpdatedAt), s.OperatorID,
		s.CurrentStep, s.SelectedUseCase, transcript,
		nullableSqliteBytes(s.ProposedConfig), nullableSqliteBytes(s.ProposedProject), nullableSqliteBytes(s.ValidationResults),
	)
	return err
}

// Get retrieves an onboarding session by ID. Returns ErrNotFound when
// the session does not exist.
func (r *InstallationOnboardingSessionRepository) Get(ctx context.Context, id string) (*persistence.InstallationOnboardingSession, error) {
	if id == "" {
		return nil, fmt.Errorf("session ID required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       current_step, selected_use_case, transcript,
		       proposed_config, proposed_project, validation_results,
		       committed_project_id, committed_at, cancelled_at
		FROM installation_onboarding_sessions
		WHERE id = ?`, id)
	return scanSqliteOnboardingSession(row)
}

// Update mutates the mutable fields of an onboarding session. Returns
// ErrNotFound when the session ID does not exist.
func (r *InstallationOnboardingSessionRepository) Update(ctx context.Context, s *persistence.InstallationOnboardingSession) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.ID == "" {
		return fmt.Errorf("session ID required")
	}
	if s.CurrentStep == "" {
		return fmt.Errorf("current step required")
	}
	if s.SelectedUseCase == "" {
		return fmt.Errorf("selected use case required")
	}
	transcript := string(s.Transcript)
	if transcript == "" {
		transcript = "[]"
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE installation_onboarding_sessions
		SET current_step = ?,
		    selected_use_case = ?,
		    transcript = ?,
		    proposed_config = ?,
		    proposed_project = ?,
		    validation_results = ?,
		    updated_at = ?
		WHERE id = ?`,
		s.CurrentStep, s.SelectedUseCase, transcript,
		nullableSqliteBytes(s.ProposedConfig), nullableSqliteBytes(s.ProposedProject), nullableSqliteBytes(s.ValidationResults),
		sqliteTime(time.Now().UTC()), s.ID,
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

// CommitTo atomically stamps a session as committed with the given
// project ID. Returns ErrInvalidTransition if already committed, or
// ErrNotFound if the session does not exist.
func (r *InstallationOnboardingSessionRepository) CommitTo(ctx context.Context, sessionID, projectID string) error {
	if sessionID == "" || projectID == "" {
		return fmt.Errorf("session ID and project ID required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE installation_onboarding_sessions
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

// Cancel marks a session as cancelled. Only the session owner can
// cancel; returns ErrNotFound for wrong owner or missing session, and
// ErrInvalidTransition if the session is already committed.
func (r *InstallationOnboardingSessionRepository) Cancel(ctx context.Context, sessionID, operatorID string) error {
	if sessionID == "" || operatorID == "" {
		return fmt.Errorf("session ID and operator ID required")
	}
	now := sqliteTime(time.Now().UTC())
	res, err := r.db.ExecContext(ctx, `
		UPDATE installation_onboarding_sessions
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

// ListByOperator returns the operator's sessions ordered by most recently
// updated first, capped at pageSize (default 20).
func (r *InstallationOnboardingSessionRepository) ListByOperator(ctx context.Context, operatorID string, pageSize int) ([]*persistence.InstallationOnboardingSession, error) {
	if operatorID == "" {
		return nil, fmt.Errorf("operator ID required")
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, created_at, updated_at, operator_id,
		       current_step, selected_use_case, transcript,
		       proposed_config, proposed_project, validation_results,
		       committed_project_id, committed_at, cancelled_at
		FROM installation_onboarding_sessions
		WHERE operator_id = ?
		ORDER BY updated_at DESC
		LIMIT ?`, operatorID, pageSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.InstallationOnboardingSession, 0)
	for rows.Next() {
		s, err := scanSqliteOnboardingSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// HasCommitted reports whether any onboarding session has been committed.
// Used by the setup-guide detector to decide whether the install is
// already onboarded.
func (r *InstallationOnboardingSessionRepository) HasCommitted(ctx context.Context) (bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM installation_onboarding_sessions
			WHERE committed_project_id IS NOT NULL
			LIMIT 1
		)`)
	var committed int
	if err := row.Scan(&committed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return committed != 0, nil
}

func scanSqliteOnboardingSession(scanner interface{ Scan(dest ...any) error }) (*persistence.InstallationOnboardingSession, error) {
	var (
		s                  persistence.InstallationOnboardingSession
		createdAt          sqlTime
		updatedAt          sqlTime
		transcript         string
		proposedConfig     sql.NullString
		proposedProject    sql.NullString
		validationResults  sql.NullString
		committedProjectID sql.NullString
		committedAt        sqlNullTime
		cancelledAt        sqlNullTime
	)
	err := scanner.Scan(
		&s.ID, &createdAt, &updatedAt, &s.OperatorID,
		&s.CurrentStep, &s.SelectedUseCase, &transcript,
		&proposedConfig, &proposedProject, &validationResults,
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
	if proposedConfig.Valid {
		s.ProposedConfig = []byte(proposedConfig.String)
	}
	if proposedProject.Valid {
		s.ProposedProject = []byte(proposedProject.String)
	}
	if validationResults.Valid {
		s.ValidationResults = []byte(validationResults.String)
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
