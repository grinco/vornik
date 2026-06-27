package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TaskScratchpadRepository is the SQLite persistence.TaskScratchpadRepository.
type TaskScratchpadRepository struct {
	db DBTX
}

func NewTaskScratchpadRepository(db DBTX) *TaskScratchpadRepository {
	return &TaskScratchpadRepository{db: db}
}

// Get returns the scratchpad for a task, or (nil, nil) when none.
func (r *TaskScratchpadRepository) Get(ctx context.Context, taskID string) (*persistence.TaskScratchpad, error) {
	if taskID == "" {
		return nil, fmt.Errorf("TaskScratchpadRepository.Get: task_id required")
	}
	var (
		sp                          persistence.TaskScratchpad
		currentPhase                sql.NullString
		lastExecID                  sql.NullString
		facts, openQs, phaseHistory sql.NullString
		updatedAt                   sqlTime
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT task_id, summary, facts, open_questions,
		       current_phase, phase_history, last_execution_id,
		       updated_at
		FROM task_scratchpads WHERE task_id = ?`,
		taskID,
	).Scan(
		&sp.TaskID, &sp.Summary, &facts, &openQs,
		&currentPhase, &phaseHistory, &lastExecID,
		&updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if facts.Valid {
		sp.Facts = []byte(facts.String)
	}
	if openQs.Valid {
		sp.OpenQuestions = []byte(openQs.String)
	}
	if phaseHistory.Valid {
		sp.PhaseHistory = []byte(phaseHistory.String)
	}
	if currentPhase.Valid {
		sp.CurrentPhase = &currentPhase.String
	}
	if lastExecID.Valid {
		sp.LastExecutionID = &lastExecID.String
	}
	sp.UpdatedAt = updatedAt.Time
	return &sp, nil
}

// Upsert writes/replaces the scratchpad. updated_at server-managed.
func (r *TaskScratchpadRepository) Upsert(ctx context.Context, sp *persistence.TaskScratchpad) error {
	if sp == nil {
		return fmt.Errorf("TaskScratchpadRepository.Upsert: nil scratchpad")
	}
	if sp.TaskID == "" {
		return fmt.Errorf("TaskScratchpadRepository.Upsert: task_id required")
	}
	now := time.Now().UTC()
	// SQLite ON CONFLICT (target) DO UPDATE SET … (3.24+) — same
	// upsert shape as Postgres without the ::jsonb casts.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO task_scratchpads (
			task_id, summary, facts, open_questions,
			current_phase, phase_history, last_execution_id, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
			summary           = excluded.summary,
			facts             = excluded.facts,
			open_questions    = excluded.open_questions,
			current_phase     = excluded.current_phase,
			phase_history     = excluded.phase_history,
			last_execution_id = excluded.last_execution_id,
			updated_at        = excluded.updated_at`,
		sp.TaskID, sp.Summary,
		nullableBlob(sp.Facts), nullableBlob(sp.OpenQuestions),
		sp.CurrentPhase, nullableBlob(sp.PhaseHistory),
		sp.LastExecutionID, sqliteTime(now),
	)
	if err != nil {
		return err
	}
	sp.UpdatedAt = now
	return nil
}
