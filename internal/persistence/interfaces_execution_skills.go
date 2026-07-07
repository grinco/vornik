package persistence

import (
	"context"
	"time"
)

// Execution → injected-skill association (LLD 2026-07-07-knowledge-
// skill-learning-loop-design §D.2). When the executor injects an
// approved knowledge skill into a role at task start it records the
// (execution_id, skill_id) pair here; when the execution completes
// successfully the maturity engine reads it back to credit a "worked"
// usage signal to exactly those skills.
//
// injected_at is write-only provenance metadata — this table is only
// ever queried by execution_id (never ordered/compared by time), so
// the standard dual-backend timestamp convention (Postgres TIMESTAMPTZ,
// SQLite ISO8601 TEXT) carries no cross-backend behaviour risk here.

// ExecutionInjectedSkill is one (execution, skill) injection record.
type ExecutionInjectedSkill struct {
	ExecutionID string
	SkillID     string
	InjectedAt  time.Time
}

// ExecutionInjectedSkillRepository is the backend-agnostic contract for
// the execution→skill association. Implemented by
// internal/persistence/{postgres,sqlite} and verified by
// repotest.RunExecutionInjectedSkillSuite.
type ExecutionInjectedSkillRepository interface {
	// Record persists one (execution_id, skill_id) association.
	// Idempotent on the composite primary key.
	Record(ctx context.Context, executionID, skillID string) error

	// ListByExecution returns the skill IDs injected into an execution
	// (empty slice when none).
	ListByExecution(ctx context.Context, executionID string) ([]string, error)
}
