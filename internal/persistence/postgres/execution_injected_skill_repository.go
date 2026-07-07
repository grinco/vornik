package postgres

import (
	"context"
)

// ExecutionInjectedSkillRepository is the PostgreSQL implementation of
// persistence.ExecutionInjectedSkillRepository. Record is idempotent on
// the (execution_id, skill_id) composite PK.
type ExecutionInjectedSkillRepository struct {
	db DBTX
}

// NewExecutionInjectedSkillRepository constructs the repo over db.
func NewExecutionInjectedSkillRepository(db DBTX) *ExecutionInjectedSkillRepository {
	return &ExecutionInjectedSkillRepository{db: db}
}

// Record inserts one association; ON CONFLICT keeps it idempotent.
func (r *ExecutionInjectedSkillRepository) Record(ctx context.Context, executionID, skillID string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_injected_skills (execution_id, skill_id)
		VALUES ($1, $2)
		ON CONFLICT (execution_id, skill_id) DO NOTHING`,
		executionID, skillID,
	)
	return mapDBError(err)
}

// ListByExecution returns the skill IDs injected into an execution.
func (r *ExecutionInjectedSkillRepository) ListByExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT skill_id FROM execution_injected_skills WHERE execution_id = $1`, executionID)
	if err != nil {
		return nil, mapDBError(err)
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
