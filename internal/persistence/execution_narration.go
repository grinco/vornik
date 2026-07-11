package persistence

import (
	"context"
	"time"
)

// ExecutionNarration is one persisted narration line produced by the
// narrator worker (https://docs.vornik.io
// §5.3). Narration is 1-to-many per step (a step can produce a start
// line, a long-tool heartbeat, and a completion line) so it gets its
// own table rather than overloading execution_step_outcomes.
//
// Seq is monotonic PER EXECUTION and assigned by the store (mirrors
// the execution_live_events precedent) so a narrator restart mid-
// execution can't reuse a seq already written — the DB computes
// MAX(seq)+1 atomically, making the sequence crash-safe even though
// the narrator's own budget/line-cap counters are not (documented
// accepted tradeoff, LLD §5.4).
type ExecutionNarration struct {
	ID          string
	ProjectID   string
	TaskID      string
	ExecutionID string
	Seq         int64
	// StepID is empty for kinds not tied to a single step
	// (milestone/completion).
	StepID string
	// Kind is one of the ExecutionNarrationKind* constants.
	Kind string
	Text string
	// Degraded flags a deterministic-fallback line (LLM unavailable,
	// timed out, or the per-execution budget capped) so the story
	// view can render a subtle "simplified" hint.
	Degraded bool
	// Metadata is a deliberate extensibility hook (JSON-encoded) —
	// later phases annotate lines (fix-it "read this", inbox "needs
	// attention") without a schema migration. Nil/empty is fine.
	Metadata  []byte
	CreatedAt time.Time
}

// ExecutionNarration.Kind values. Mirrors the design's
// `kind (step|tool|milestone|completion)` column.
const (
	ExecutionNarrationKindStep       = "step"
	ExecutionNarrationKindTool       = "tool"
	ExecutionNarrationKindMilestone  = "milestone"
	ExecutionNarrationKindCompletion = "completion"
)

// ExecutionNarrationRepository persists the narrator's plain-
// language story per execution. Implementations:
//
//   - Postgres: real persistence, FK ON DELETE CASCADE to
//     executions(id) — narration rows die with their execution.
//   - SQLite: real persistence with the same schema shape.
//     Foreign keys are declared but not enforced on this backend
//     (project-wide convention, see sqlite/sqlite.go) so cascade
//     delete is a Postgres-only guarantee; single-process SQLite
//     deployments accept the orphan-row edge case the same way
//     every other cascading table on this backend does.
type ExecutionNarrationRepository interface {
	// Insert persists one narration row, computing the next
	// per-execution seq atomically (any caller-set Seq is
	// ignored). Returns the assigned seq.
	Insert(ctx context.Context, row *ExecutionNarration) (seq int64, err error)

	// ListByExecution returns every narration row for executionID,
	// ordered by seq ascending — the story in emission order.
	ListByExecution(ctx context.Context, executionID string) ([]*ExecutionNarration, error)
}
