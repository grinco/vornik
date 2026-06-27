package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// AutonomyEvaluationRepository persists per-tick autonomy decisions.
type AutonomyEvaluationRepository struct {
	db DBTX
}

func NewAutonomyEvaluationRepository(db DBTX) *AutonomyEvaluationRepository {
	return &AutonomyEvaluationRepository{db: db}
}

// Record inserts one evaluation row.
func (r *AutonomyEvaluationRepository) Record(ctx context.Context, e *persistence.AutonomyEvaluation) error {
	if e == nil {
		return fmt.Errorf("autonomy evaluation is nil")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO autonomy_evaluations (
			id, project_id, outcome, reason, task_id,
			task_type, workflow_id, prompt_hash, duration_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ProjectID, e.Outcome, e.Reason, e.TaskID,
		e.TaskType, e.WorkflowID, e.PromptHash, e.DurationMs, sqliteTime(e.CreatedAt),
	)
	return err
}

// List returns rows matching filter, newest-first.
func (r *AutonomyEvaluationRepository) List(ctx context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	var b strings.Builder
	b.WriteString(`
		SELECT id, project_id, outcome, reason, task_id,
		       task_type, workflow_id, prompt_hash, duration_ms, created_at
		FROM autonomy_evaluations WHERE 1=1`)
	args := make([]any, 0, 4)
	if filter.ProjectID != nil {
		b.WriteString(" AND project_id = ?")
		args = append(args, *filter.ProjectID)
	}
	if filter.Outcome != nil {
		b.WriteString(" AND outcome = ?")
		args = append(args, *filter.Outcome)
	}
	b.WriteString(" ORDER BY created_at DESC")
	if filter.PageSize > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, filter.PageSize)
	}
	if filter.Offset > 0 {
		b.WriteString(" OFFSET ?")
		args = append(args, filter.Offset)
	}
	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.AutonomyEvaluation
	for rows.Next() {
		var (
			e         persistence.AutonomyEvaluation
			taskID    sql.NullString
			createdAt sqlTime
		)
		if err := rows.Scan(
			&e.ID, &e.ProjectID, &e.Outcome, &e.Reason, &taskID,
			&e.TaskType, &e.WorkflowID, &e.PromptHash, &e.DurationMs, &createdAt,
		); err != nil {
			return nil, err
		}
		if taskID.Valid {
			e.TaskID = &taskID.String
		}
		e.CreatedAt = createdAt.Time
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountByOutcome groups rows by outcome within a window.
func (r *AutonomyEvaluationRepository) CountByOutcome(ctx context.Context, projectID string, since, until time.Time) (map[string]int64, error) {
	var b strings.Builder
	b.WriteString(`SELECT outcome, COUNT(*) FROM autonomy_evaluations WHERE project_id = ?`)
	args := []any{projectID}
	if !since.IsZero() {
		b.WriteString(" AND created_at >= ?")
		args = append(args, sqliteTime(since))
	}
	if !until.IsZero() {
		b.WriteString(" AND created_at < ?")
		args = append(args, sqliteTime(until))
	}
	b.WriteString(" GROUP BY outcome")

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
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
