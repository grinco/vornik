package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskJudgeVerdictRepository persists Phase-3 judge verdicts.
type TaskJudgeVerdictRepository struct {
	db DBTX
}

func NewTaskJudgeVerdictRepository(db DBTX) *TaskJudgeVerdictRepository {
	return &TaskJudgeVerdictRepository{db: db}
}

// Record persists a verdict row; ErrDuplicateKey on second write
// for the same task. Mirrors the Postgres pre-insert idempotency
// check (one verdict per task).
func (r *TaskJudgeVerdictRepository) Record(ctx context.Context, v *persistence.TaskJudgeVerdict) error {
	if v == nil {
		return fmt.Errorf("nil verdict")
	}
	if v.RecordedAt.IsZero() {
		v.RecordedAt = time.Now().UTC()
	}
	var exists int
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM task_judge_verdicts WHERE task_id = ?)`, v.TaskID,
	).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return persistence.ErrDuplicateKey
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_judge_verdicts (
			id, project_id, task_id, role, model, verdict,
			confidence, signals, summary, cost_usd, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.ProjectID, v.TaskID, v.Role, v.Model, v.Verdict,
		v.Confidence, nullableBlob(v.Signals), v.Summary, v.CostUSD, sqliteTime(v.RecordedAt),
	)
	return err
}

// GetByTask returns the single verdict for a task, ErrNotFound otherwise.
func (r *TaskJudgeVerdictRepository) GetByTask(ctx context.Context, taskID string) (*persistence.TaskJudgeVerdict, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, task_id, role, model, verdict,
		       confidence, signals, summary, cost_usd, recorded_at
		FROM task_judge_verdicts WHERE task_id = ?
		ORDER BY recorded_at DESC LIMIT 1`, taskID)
	return scanJudgeVerdict(row)
}

// ListRecent returns the newest verdicts (capped) for a project,
// or globally when projectID is empty.
func (r *TaskJudgeVerdictRepository) ListRecent(ctx context.Context, projectID string, limit int) ([]*persistence.TaskJudgeVerdict, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if projectID != "" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, project_id, task_id, role, model, verdict,
			       confidence, signals, summary, cost_usd, recorded_at
			FROM task_judge_verdicts
			WHERE project_id = ?
			ORDER BY recorded_at DESC
			LIMIT ?`, projectID, limit)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, project_id, task_id, role, model, verdict,
			       confidence, signals, summary, cost_usd, recorded_at
			FROM task_judge_verdicts
			ORDER BY recorded_at DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*persistence.TaskJudgeVerdict
	for rows.Next() {
		v, err := scanJudgeVerdict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanJudgeVerdict(scanner interface{ Scan(dest ...any) error }) (*persistence.TaskJudgeVerdict, error) {
	v := &persistence.TaskJudgeVerdict{}
	var (
		signals    sql.NullString
		recordedAt sqlTime
	)
	err := scanner.Scan(
		&v.ID, &v.ProjectID, &v.TaskID, &v.Role, &v.Model, &v.Verdict,
		&v.Confidence, &signals, &v.Summary, &v.CostUSD, &recordedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.ErrNotFound
		}
		return nil, err
	}
	if signals.Valid {
		v.Signals = []byte(signals.String)
	}
	v.RecordedAt = recordedAt.Time
	return v, nil
}
