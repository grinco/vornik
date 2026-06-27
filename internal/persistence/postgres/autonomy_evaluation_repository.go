package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AutonomyEvaluationRepository persists per-tick autonomy evaluation rows.
// Write path is called from internal/autonomy at every tick termination;
// read path powers the API + vornikctl autonomy evaluations CLI.
type AutonomyEvaluationRepository struct {
	db DBTX
}

// NewAutonomyEvaluationRepository creates a new repository.
func NewAutonomyEvaluationRepository(db DBTX) *AutonomyEvaluationRepository {
	return &AutonomyEvaluationRepository{db: db}
}

// Record inserts a single evaluation row. The autonomy loop writes this
// on every tick regardless of outcome, so it runs inside the tick's
// context — callers are expected to use a bounded detached context when
// the tick context is already cancelled (e.g. LLM error on shutdown).
func (r *AutonomyEvaluationRepository) Record(ctx context.Context, e *persistence.AutonomyEvaluation) error {
	if e == nil {
		return fmt.Errorf("autonomy evaluation is nil")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	var taskID sql.NullString
	if e.TaskID != nil {
		taskID = sql.NullString{String: *e.TaskID, Valid: true}
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO autonomy_evaluations (
			id, project_id, outcome, reason, task_id,
			task_type, workflow_id, prompt_hash, duration_ms, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		e.ID, e.ProjectID, e.Outcome, e.Reason, taskID,
		e.TaskType, e.WorkflowID, e.PromptHash, e.DurationMs, e.CreatedAt,
	)
	return mapDBError(err)
}

// List returns evaluation rows matching the filter, newest first. Used by
// the audit API and CLI. A project-scoped query with no outcome filter is
// the common shape — the index idx_autonomy_eval_project_time covers it.
func (r *AutonomyEvaluationRepository) List(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	query := `
		SELECT id, project_id, outcome, reason, task_id,
		       task_type, workflow_id, prompt_hash, duration_ms, created_at
		FROM autonomy_evaluations
		WHERE 1=1
	`
	args := make([]any, 0, 4)
	argPos := 1
	if filter.ProjectID != nil {
		query += fmt.Sprintf(" AND project_id = $%d", argPos)
		args = append(args, *filter.ProjectID)
		argPos++
	}
	if filter.Outcome != nil {
		query += fmt.Sprintf(" AND outcome = $%d", argPos)
		args = append(args, *filter.Outcome)
		argPos++
	}
	query += " ORDER BY created_at DESC"
	if filter.PageSize > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argPos)
		args = append(args, filter.PageSize)
		argPos++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(" OFFSET $%d", argPos)
		args = append(args, filter.Offset)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.AutonomyEvaluation
	for rows.Next() {
		e, err := scanAutonomyEvaluation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountByOutcome groups evaluations by outcome within an optional time
// window. Zero time is unbounded on that side. Single COUNT aggregate per
// row — no N+1.
func (r *AutonomyEvaluationRepository) CountByOutcome(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
	query := `
		SELECT outcome, COUNT(*)
		FROM autonomy_evaluations
		WHERE project_id = $1
	`
	args := []any{projectID}
	argPos := 2
	if !since.IsZero() {
		query += fmt.Sprintf(" AND created_at >= $%d", argPos)
		args = append(args, since)
		argPos++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND created_at < $%d", argPos)
		args = append(args, until)
	}
	query += " GROUP BY outcome"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			return nil, err
		}
		out[outcome] = count
	}
	return out, rows.Err()
}

func scanAutonomyEvaluation(scanner interface {
	Scan(dest ...any) error
}) (*persistence.AutonomyEvaluation, error) {
	var e persistence.AutonomyEvaluation
	var taskID sql.NullString
	if err := scanner.Scan(
		&e.ID, &e.ProjectID, &e.Outcome, &e.Reason, &taskID,
		&e.TaskType, &e.WorkflowID, &e.PromptHash, &e.DurationMs, &e.CreatedAt,
	); err != nil {
		return nil, mapDBError(err)
	}
	if taskID.Valid {
		e.TaskID = &taskID.String
	}
	return &e, nil
}
