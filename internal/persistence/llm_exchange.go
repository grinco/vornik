package persistence

import (
	"context"
	"time"
)

// LLMExchange is one model request/response pair of an agent step, recorded
// at the daemon's chat proxy when the execution's project opted in
// (2026-09-04-llm-exchange-record-replay-design.md §2–§3). Both bodies are
// stored AFTER redaction; RequestHash is the sha256 of the stored canonical
// request, which is what a replay matches on.
type LLMExchange struct {
	ID          string
	ExecutionID string
	StepID      string
	// Iteration is the loop counter the container sent, nil when the image
	// predates the header.
	Iteration *int
	// Seq is the arrival order within (execution, step); the store assigns it.
	Seq              int
	RequestHash      string
	RequestJSON      string
	ResponseJSON     string
	Model            string
	PromptTokens     int
	CompletionTokens int
	DurationMs       int
	// Redactions is how many secret findings the redaction seam replaced in
	// the two bodies before storage; a replay miss on such a row is the
	// designed consequence of §4, not a loop divergence.
	Redactions int
	RecordedAt time.Time
}

// LLMExchangeRepository persists llm_exchanges (migration 177; SQLite parity
// in schema.go). A row lives exactly as long as its execution: retention
// step 5e removes every row whose execution is gone, on both backends,
// because SQLite runs with foreign keys off.
type LLMExchangeRepository interface {
	// Record inserts one exchange and assigns Seq (and ID, RecordedAt when
	// zero). Bodies are stored as given — the caller redacts first.
	Record(ctx context.Context, x *LLMExchange) error
	// ListByStep returns the step's exchanges in Seq order. An unrecorded
	// step is an empty, non-nil slice — the truthful answer, since the
	// project may not have opted in.
	ListByStep(ctx context.Context, executionID, stepID string) ([]*LLMExchange, error)
	// CountByExecution reports how many exchanges the execution has, so a
	// page can offer the link only when there is something behind it.
	CountByExecution(ctx context.Context, executionID string) (int, error)
}
