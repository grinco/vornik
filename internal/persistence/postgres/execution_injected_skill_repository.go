package postgres

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
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
//
// An empty bodySHA256 lands as NULL rather than as the empty string, so
// "we never recorded which body ran" stays distinguishable from any real
// sha in the arm queries (migration 180).
func (r *ExecutionInjectedSkillRepository) Record(ctx context.Context, executionID, skillID, bodySHA256 string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO execution_injected_skills (execution_id, skill_id, body_sha256)
		VALUES ($1, $2, $3)
		ON CONFLICT (execution_id, skill_id) DO NOTHING`,
		executionID, skillID, pgNullStr(bodySHA256),
	)
	return mapDBError(err)
}

// SkillInjectionProvenance splits this skill's injections by body.
//
// The join to executions is what applies the window: the association row
// carries an injected_at, but the rollup's window is over execution
// created_at, and two different clocks over the same window is how the
// provenance counts and the arms come to describe different sets of runs.
func (r *ExecutionInjectedSkillRepository) SkillInjectionProvenance(
	ctx context.Context, skillID, bodySHA256 string, since time.Time,
) (persistence.InjectionProvenance, error) {
	var out persistence.InjectionProvenance
	err := r.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN i.body_sha256 = $2 THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN i.body_sha256 IS NOT NULL AND i.body_sha256 <> $2 THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN i.body_sha256 IS NULL THEN 1 ELSE 0 END), 0)
		  FROM execution_injected_skills i
		  JOIN executions e ON e.id = i.execution_id
		 WHERE i.skill_id = $1 AND e.created_at >= $3`,
		skillID, bodySHA256, since,
	).Scan(&out.MatchingN, &out.OtherBodyN, &out.UnknownBodyN)
	if err != nil {
		return persistence.InjectionProvenance{}, mapDBError(err)
	}
	return out, nil
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
