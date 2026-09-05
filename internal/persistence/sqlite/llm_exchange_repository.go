package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// LLMExchangeRepository implements persistence.LLMExchangeRepository over
// llm_exchanges (migration 177 parity in schema.go).
type LLMExchangeRepository struct {
	db *sql.DB
}

// NewLLMExchangeRepository wires the repository.
func NewLLMExchangeRepository(db *sql.DB) *LLMExchangeRepository {
	return &LLMExchangeRepository{db: db}
}

// Record inserts the exchange, assigning seq as one past the step's current
// maximum in the same statement.
func (r *LLMExchangeRepository) Record(ctx context.Context, x *persistence.LLMExchange) error {
	if x == nil {
		return fmt.Errorf("sqlite: llm_exchanges: nil exchange")
	}
	if x.ExecutionID == "" || x.StepID == "" {
		return fmt.Errorf("sqlite: llm_exchanges: execution_id and step_id required")
	}
	if x.ID == "" {
		x.ID = persistence.GenerateID("llmx")
	}
	if x.RecordedAt.IsZero() {
		x.RecordedAt = time.Now().UTC()
	}
	var iteration any
	if x.Iteration != nil {
		iteration = *x.Iteration
	}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO llm_exchanges (
			id, execution_id, step_id, iteration, seq, request_hash, request_json, response_json,
			model, prompt_tokens, completion_tokens, duration_ms, redactions, recorded_at
		) VALUES (
			?, ?, ?, ?,
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM llm_exchanges WHERE execution_id = ? AND step_id = ?),
			?, ?, ?, ?, ?, ?, ?, ?, ?
		) RETURNING seq`,
		x.ID, x.ExecutionID, x.StepID, iteration, x.ExecutionID, x.StepID,
		x.RequestHash, x.RequestJSON, x.ResponseJSON,
		x.Model, x.PromptTokens, x.CompletionTokens, x.DurationMs, x.Redactions, sqliteTime(x.RecordedAt),
	).Scan(&x.Seq)
	if err != nil {
		return fmt.Errorf("sqlite: llm_exchanges record: %w", err)
	}
	return nil
}

// ListByStep returns the step's exchanges in seq order; empty, never nil.
func (r *LLMExchangeRepository) ListByStep(ctx context.Context, executionID, stepID string) ([]*persistence.LLMExchange, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, execution_id, step_id, iteration, seq, request_hash, request_json, response_json,
		       model, prompt_tokens, completion_tokens, duration_ms, redactions, recorded_at
		FROM llm_exchanges WHERE execution_id = ? AND step_id = ? ORDER BY seq ASC`, executionID, stepID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: llm_exchanges list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.LLMExchange, 0)
	for rows.Next() {
		var x persistence.LLMExchange
		var iteration sql.NullInt64
		var recorded sqlTime
		if err := rows.Scan(&x.ID, &x.ExecutionID, &x.StepID, &iteration, &x.Seq, &x.RequestHash, &x.RequestJSON, &x.ResponseJSON,
			&x.Model, &x.PromptTokens, &x.CompletionTokens, &x.DurationMs, &x.Redactions, &recorded); err != nil {
			return nil, fmt.Errorf("sqlite: llm_exchanges scan: %w", err)
		}
		if iteration.Valid {
			v := int(iteration.Int64)
			x.Iteration = &v
		}
		x.RecordedAt = recorded.Time
		out = append(out, &x)
	}
	return out, rows.Err()
}

// CountByExecution reports the execution's exchange count.
func (r *LLMExchangeRepository) CountByExecution(ctx context.Context, executionID string) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_exchanges WHERE execution_id = ?`, executionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: llm_exchanges count: %w", err)
	}
	return n, nil
}
