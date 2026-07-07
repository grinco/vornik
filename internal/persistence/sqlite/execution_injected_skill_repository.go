package sqlite

import (
	"context"
	"time"
)

// ExecutionInjectedSkillRepository is the SQLite implementation of
// persistence.ExecutionInjectedSkillRepository. Record is idempotent on
// the (execution_id, skill_id) composite PK.
type ExecutionInjectedSkillRepository struct {
	db DBTX
}

// NewExecutionInjectedSkillRepository constructs the repo over db.
func NewExecutionInjectedSkillRepository(db DBTX) *ExecutionInjectedSkillRepository {
	return &ExecutionInjectedSkillRepository{db: db}
}

// Record inserts one association. INSERT OR IGNORE keeps it idempotent.
func (r *ExecutionInjectedSkillRepository) Record(ctx context.Context, executionID, skillID string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO execution_injected_skills (execution_id, skill_id, injected_at)
		VALUES (?, ?, ?)`,
		executionID, skillID, sqliteTime(time.Now().UTC()),
	)
	return err
}

// ListByExecution returns the skill IDs injected into an execution.
func (r *ExecutionInjectedSkillRepository) ListByExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT skill_id FROM execution_injected_skills WHERE execution_id = ?`, executionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
