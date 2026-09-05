package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LLMExchangeRepository implements persistence.LLMExchangeRepository over
// llm_exchanges (migration 177).
type LLMExchangeRepository struct {
	db persistence.DBTX
}

// NewLLMExchangeRepository wires the repository.
func NewLLMExchangeRepository(db persistence.DBTX) *LLMExchangeRepository {
	return &LLMExchangeRepository{db: db}
}

// Record inserts the exchange, assigning seq as one past the step's current
// maximum. The loop that produces exchanges is sequential, so the
// read-then-insert inside one statement is not raced by a sibling.
func (r *LLMExchangeRepository) Record(ctx context.Context, x *persistence.LLMExchange) error {
	if x == nil {
		return fmt.Errorf("postgres: llm_exchanges: nil exchange")
	}
	if x.ExecutionID == "" || x.StepID == "" {
		return fmt.Errorf("postgres: llm_exchanges: execution_id and step_id required")
	}
	if x.ID == "" {
		x.ID = persistence.GenerateID("llmx")
	}
	if x.RecordedAt.IsZero() {
		x.RecordedAt = time.Now().UTC()
	}
	var iteration sql.NullInt64
	if x.Iteration != nil {
		iteration = sql.NullInt64{Int64: int64(*x.Iteration), Valid: true}
	}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO llm_exchanges (
			id, execution_id, step_id, iteration, seq, request_hash, request_json, response_json,
			model, prompt_tokens, completion_tokens, duration_ms, redactions, recorded_at
		) VALUES (
			$1, $2, $3, $4,
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM llm_exchanges WHERE execution_id = $2 AND step_id = $3),
			$5, $6, $7, $8, $9, $10, $11, $12, $13
		) RETURNING seq`,
		x.ID, x.ExecutionID, x.StepID, iteration, x.RequestHash, x.RequestJSON, x.ResponseJSON,
		x.Model, x.PromptTokens, x.CompletionTokens, x.DurationMs, x.Redactions, x.RecordedAt,
	).Scan(&x.Seq)
	if err != nil {
		return fmt.Errorf("postgres: llm_exchanges record: %w", mapDBError(err))
	}
	return nil
}

const llmExchangeColumns = `id, execution_id, step_id, iteration, seq, request_hash, request_json, response_json,
	model, prompt_tokens, completion_tokens, duration_ms, redactions, recorded_at`

// ListByStep returns the step's exchanges in seq order; empty, never nil.
func (r *LLMExchangeRepository) ListByStep(ctx context.Context, executionID, stepID string) ([]*persistence.LLMExchange, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+llmExchangeColumns+`
		FROM llm_exchanges WHERE execution_id = $1 AND step_id = $2 ORDER BY seq ASC`, executionID, stepID)
	if err != nil {
		return nil, fmt.Errorf("postgres: llm_exchanges list: %w", mapDBError(err))
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.LLMExchange, 0)
	for rows.Next() {
		var x persistence.LLMExchange
		var iteration sql.NullInt64
		if err := rows.Scan(&x.ID, &x.ExecutionID, &x.StepID, &iteration, &x.Seq, &x.RequestHash, &x.RequestJSON, &x.ResponseJSON,
			&x.Model, &x.PromptTokens, &x.CompletionTokens, &x.DurationMs, &x.Redactions, &x.RecordedAt); err != nil {
			return nil, fmt.Errorf("postgres: llm_exchanges scan: %w", err)
		}
		if iteration.Valid {
			v := int(iteration.Int64)
			x.Iteration = &v
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

// CountByExecution reports the execution's exchange count.
func (r *LLMExchangeRepository) CountByExecution(ctx context.Context, executionID string) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_exchanges WHERE execution_id = $1`, executionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: llm_exchanges count: %w", mapDBError(err))
	}
	return n, nil
}
