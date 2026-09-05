package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// StepPromptRepository implements persistence.StepPromptRepository over
// step_prompts (migration 175).
type StepPromptRepository struct {
	db persistence.DBTX
}

// NewStepPromptRepository wires the repository.
func NewStepPromptRepository(db persistence.DBTX) *StepPromptRepository {
	return &StepPromptRepository{db: db}
}

// Save stores a part under the sha256 of its bytes; a second Save of the
// same bytes is a no-op returning the same hash.
func (r *StepPromptRepository) Save(ctx context.Context, part persistence.StepPromptPart, body string) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("postgres: step_prompts: no database handle")
	}
	if part == "" {
		return "", fmt.Errorf("postgres: step_prompts: part required")
	}
	hash := persistence.HashStepPrompt(body)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO step_prompts (hash, part, body, bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (hash) DO NOTHING`, hash, string(part), body, len(body))
	if err != nil {
		return "", fmt.Errorf("postgres: step_prompts save: %w", mapDBError(err))
	}
	return hash, nil
}

// Get fetches one part by hash.
func (r *StepPromptRepository) Get(ctx context.Context, hash string) (*persistence.StepPrompt, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("postgres: step_prompts: no database handle")
	}
	var p persistence.StepPrompt
	var part string
	err := r.db.QueryRowContext(ctx, `SELECT hash, part, body FROM step_prompts WHERE hash = $1`, hash).
		Scan(&p.Hash, &part, &p.Body)
	if err == sql.ErrNoRows {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: step_prompts get: %w", mapDBError(err))
	}
	p.Part = persistence.StepPromptPart(part)
	return &p, nil
}

// PruneUnreferenced deletes every part no surviving outcome row points at
// through any of its three hash columns. The partial indexes from migration
// 175 make each NOT EXISTS an index probe.
func (r *StepPromptRepository) PruneUnreferenced(ctx context.Context) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("postgres: step_prompts: no database handle")
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM step_prompts p
		WHERE NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_system_hash = p.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_user_hash = p.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_tools_hash = p.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.input_hash = p.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.result_hash = p.hash)`)
	if err != nil {
		return 0, fmt.Errorf("postgres: step_prompts prune: %w", mapDBError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

var _ persistence.StepPromptRepository = (*StepPromptRepository)(nil)
