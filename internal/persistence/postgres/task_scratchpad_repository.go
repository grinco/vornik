package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskScratchpadRepository is the Postgres backing for the
// task_scratchpad table introduced in migration v24. One row per
// task — the lead's running summary, updated at the end of every
// execution and read at the start of every subsequent execution.
//
// See https://docs.vornik.io
// §4.2.
type TaskScratchpadRepository struct {
	db DBTX
}

// NewTaskScratchpadRepository constructs the repo over a DBTX.
func NewTaskScratchpadRepository(db DBTX) *TaskScratchpadRepository {
	return &TaskScratchpadRepository{db: db}
}

// Get returns the scratchpad for a task. Returns nil + nil error
// when the task has none yet (never been touched by a lead under
// the new lifecycle).
func (r *TaskScratchpadRepository) Get(ctx context.Context, taskID string) (*persistence.TaskScratchpad, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TaskScratchpadRepository.Get: task_id required")
	}
	var sp persistence.TaskScratchpad
	var facts, openQs, phaseHist sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT task_id, summary, facts, open_questions,
		       current_phase, phase_history, last_execution_id,
		       updated_at
		FROM task_scratchpad WHERE task_id = $1
	`, taskID).Scan(
		&sp.TaskID, &sp.Summary, &facts, &openQs,
		&sp.CurrentPhase, &phaseHist, &sp.LastExecutionID,
		&sp.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, mapDBError(err)
	}
	if facts.Valid {
		sp.Facts = []byte(facts.String)
	}
	if openQs.Valid {
		sp.OpenQuestions = []byte(openQs.String)
	}
	if phaseHist.Valid {
		sp.PhaseHistory = []byte(phaseHist.String)
	}
	return &sp, nil
}

// Upsert writes or replaces the scratchpad row for a task.
// updated_at is server-managed (now()); caller-provided value is
// ignored to keep "freshest write wins" simple under concurrent
// updaters.
func (r *TaskScratchpadRepository) Upsert(ctx context.Context, sp *persistence.TaskScratchpad) error {
	if sp == nil {
		return fmt.Errorf("TaskScratchpadRepository.Upsert: nil scratchpad")
	}
	if sp.TaskID == "" {
		return fmt.Errorf("TaskScratchpadRepository.Upsert: task_id required")
	}
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_scratchpad (
			task_id, summary, facts, open_questions,
			current_phase, phase_history, last_execution_id, updated_at
		) VALUES ($1, $2,
		          COALESCE($3::jsonb, '{}'::jsonb),
		          COALESCE($4::jsonb, '[]'::jsonb),
		          $5,
		          COALESCE($6::jsonb, '[]'::jsonb),
		          $7, $8)
		ON CONFLICT (task_id) DO UPDATE SET
			summary           = EXCLUDED.summary,
			facts             = EXCLUDED.facts,
			open_questions    = EXCLUDED.open_questions,
			current_phase     = EXCLUDED.current_phase,
			phase_history     = EXCLUDED.phase_history,
			last_execution_id = EXCLUDED.last_execution_id,
			updated_at        = EXCLUDED.updated_at
	`,
		sp.TaskID, sp.Summary,
		jsonOrNull(sp.Facts),
		jsonOrNull(sp.OpenQuestions),
		sp.CurrentPhase,
		jsonOrNull(sp.PhaseHistory),
		sp.LastExecutionID, now,
	)
	if err != nil {
		return mapDBError(err)
	}
	sp.UpdatedAt = now
	return nil
}
