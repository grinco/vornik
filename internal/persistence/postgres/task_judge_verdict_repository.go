package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskJudgeVerdictRepository implements
// persistence.TaskJudgeVerdictRepository on PostgreSQL.
type TaskJudgeVerdictRepository struct {
	db DBTX
}

// NewTaskJudgeVerdictRepository constructs a new repo over db.
func NewTaskJudgeVerdictRepository(db DBTX) *TaskJudgeVerdictRepository {
	return &TaskJudgeVerdictRepository{db: db}
}

// Record persists a verdict row. Returns persistence.ErrAlreadyExists
// when a verdict already exists for this task — the unique-by-task
// invariant is enforced at the application layer (no UNIQUE
// constraint on the column, since a future "re-judge" path may
// want to write a second row tagged superseded). For now the
// caller treats AlreadyExists as a no-op signal.
func (r *TaskJudgeVerdictRepository) Record(ctx context.Context, v *persistence.TaskJudgeVerdict) error {
	if v == nil {
		return fmt.Errorf("nil verdict")
	}
	recordedAt := v.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	// Cheap pre-insert idempotency check: a separate row already
	// for this task short-circuits with ErrAlreadyExists. Race-
	// safe enough for the async-judge path because the runner
	// only fires once per task; the hot path is effectively
	// single-writer.
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM task_judge_verdicts WHERE task_id = $1)`, v.TaskID,
	).Scan(&exists); err != nil {
		return mapDBError(err)
	}
	if exists {
		return persistence.ErrDuplicateKey
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_judge_verdicts (
		    id, project_id, task_id, role, model, verdict,
		    confidence, signals, summary, cost_usd, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		v.ID, v.ProjectID, v.TaskID, v.Role, v.Model, v.Verdict,
		v.Confidence, nullableJSONB(v.Signals), v.Summary, v.CostUSD, recordedAt,
	)
	return mapDBError(err)
}

// GetByTask fetches the single verdict for a task, or
// persistence.ErrNotFound when none exists.
func (r *TaskJudgeVerdictRepository) GetByTask(ctx context.Context, taskID string) (*persistence.TaskJudgeVerdict, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, task_id, role, model, verdict,
		       confidence, signals, summary, cost_usd, recorded_at
		FROM task_judge_verdicts WHERE task_id = $1
		ORDER BY recorded_at DESC LIMIT 1`, taskID)
	v := &persistence.TaskJudgeVerdict{}
	var signals []byte
	if err := row.Scan(
		&v.ID, &v.ProjectID, &v.TaskID, &v.Role, &v.Model, &v.Verdict,
		&v.Confidence, &signals, &v.Summary, &v.CostUSD, &v.RecordedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, mapDBError(err)
	}
	if len(signals) > 0 {
		v.Signals = signals
	}
	return v, nil
}

// ListRecent returns the newest verdicts for a project (or
// every project when projectID is empty), capped at limit.
// Powers the rollup tile + dashboards.
func (r *TaskJudgeVerdictRepository) ListRecent(ctx context.Context, projectID string, limit int) ([]*persistence.TaskJudgeVerdict, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT id, project_id, task_id, role, model, verdict,
		       confidence, signals, summary, cost_usd, recorded_at
		FROM task_judge_verdicts`
	args := make([]any, 0, 2)
	if projectID != "" {
		query += " WHERE project_id = $1"
		args = append(args, projectID)
	}
	query += " ORDER BY recorded_at DESC"
	if projectID != "" {
		query += " LIMIT $2"
	} else {
		query += " LIMIT $1"
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.TaskJudgeVerdict
	for rows.Next() {
		v := &persistence.TaskJudgeVerdict{}
		var signals []byte
		if err := rows.Scan(
			&v.ID, &v.ProjectID, &v.TaskID, &v.Role, &v.Model, &v.Verdict,
			&v.Confidence, &signals, &v.Summary, &v.CostUSD, &v.RecordedAt,
		); err != nil {
			return nil, err
		}
		if len(signals) > 0 {
			v.Signals = signals
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
