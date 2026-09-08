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
	// BodySHA256 is the sha256 of the skill body actually injected. Empty for
	// rows written before Postgres migration 180, where the body is
	// unknowable — never assumed to match the body under review.
	BodySHA256 string
}

// InjectionProvenance counts a skill's injections in a window by how the body
// each execution ran with relates to the body an operator is deciding about
// (2026-09-08-execution-ratings-approval-paths-design.md §4).
//
// The three counts are what let a surface say WHICH kind of "no usable
// evidence" it is looking at: evidence for this body, evidence for a body that
// has been replaced, or evidence that cannot be tied to any body at all. They
// are different facts and an operator acts differently on each.
type InjectionProvenance struct {
	// MatchingN is executions that ran the body under review.
	MatchingN int
	// OtherBodyN is executions that ran a DIFFERENT, recorded body.
	OtherBodyN int
	// UnknownBodyN is executions whose body was never recorded (pre-migration).
	UnknownBodyN int
}

// ExecutionInjectedSkillRepository is the backend-agnostic contract for
// the execution→skill association. Implemented by
// internal/persistence/{postgres,sqlite} and verified by
// repotest.RunExecutionInjectedSkillSuite.
type ExecutionInjectedSkillRepository interface {
	// Record persists one (execution_id, skill_id) association together with
	// the sha256 of the body that was injected. Idempotent on the composite
	// primary key. An empty bodySHA256 is stored as NULL — unknown provenance,
	// which the rollup must never read as a match.
	Record(ctx context.Context, executionID, skillID, bodySHA256 string) error

	// SkillInjectionProvenance counts this skill's injections since `since`,
	// split by how each execution's recorded body relates to bodySHA256.
	SkillInjectionProvenance(ctx context.Context, skillID, bodySHA256 string, since time.Time) (InjectionProvenance, error)

	// ListByExecution returns the skill IDs injected into an execution
	// (empty slice when none).
	ListByExecution(ctx context.Context, executionID string) ([]string, error)
}
