package sqlite

import (
	"context"
	"database/sql"
	"time"

	"vornik.io/vornik/internal/persistence"
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
//
// An empty bodySHA256 lands as NULL, matching the Postgres side: "we never
// recorded which body ran" must stay distinguishable from a real sha, or the
// arm query would count a historical row as a match for the body under review
// (migration 180).
func (r *ExecutionInjectedSkillRepository) Record(ctx context.Context, executionID, skillID, bodySHA256 string) error {
	var sha any
	if bodySHA256 != "" {
		sha = bodySHA256
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO execution_injected_skills (execution_id, skill_id, injected_at, body_sha256)
		VALUES (?, ?, ?, ?)`,
		executionID, skillID, sqliteTime(time.Now().UTC()), sha,
	)
	return err
}

// SkillInjectionProvenance splits this skill's injections by body.
//
// Windowed on the EXECUTION's created_at rather than the association's
// injected_at, matching Postgres: the arms are windowed that way, and two
// clocks over one window is how the provenance counts and the arms come to
// describe different sets of runs.
func (r *ExecutionInjectedSkillRepository) SkillInjectionProvenance(
	ctx context.Context, skillID, bodySHA256 string, since time.Time,
) (persistence.InjectionProvenance, error) {
	var out persistence.InjectionProvenance
	err := r.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN i.body_sha256 = ? THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN i.body_sha256 IS NOT NULL AND i.body_sha256 <> ? THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN i.body_sha256 IS NULL THEN 1 ELSE 0 END), 0)
		  FROM execution_injected_skills i
		  JOIN executions e ON e.id = i.execution_id
		 WHERE i.skill_id = ? AND e.created_at >= ?`,
		bodySHA256, bodySHA256, skillID, sqliteTime(since),
	).Scan(&out.MatchingN, &out.OtherBodyN, &out.UnknownBodyN)
	if err != nil && err != sql.ErrNoRows {
		return persistence.InjectionProvenance{}, err
	}
	return out, nil
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
