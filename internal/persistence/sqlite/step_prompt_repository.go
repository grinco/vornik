package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// StepPromptRepository implements persistence.StepPromptRepository over
// step_prompts (migration 175 parity in schema.go).
type StepPromptRepository struct {
	db *sql.DB
}

// NewStepPromptRepository wires the repository.
func NewStepPromptRepository(db *sql.DB) *StepPromptRepository {
	return &StepPromptRepository{db: db}
}

// Save stores a part under the sha256 of its bytes; a second Save of the
// same bytes is a no-op returning the same hash.
func (r *StepPromptRepository) Save(ctx context.Context, part persistence.StepPromptPart, body string) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("sqlite: step_prompts: no database handle")
	}
	if part == "" {
		return "", fmt.Errorf("sqlite: step_prompts: part required")
	}
	hash := persistence.HashStepPrompt(body)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO step_prompts (hash, part, body, bytes, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (hash) DO NOTHING`, hash, string(part), body, len(body), sqliteTime(time.Now()))
	if err != nil {
		return "", fmt.Errorf("sqlite: step_prompts save: %w", err)
	}
	return hash, nil
}

// Get fetches one part by hash.
func (r *StepPromptRepository) Get(ctx context.Context, hash string) (*persistence.StepPrompt, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("sqlite: step_prompts: no database handle")
	}
	var p persistence.StepPrompt
	var part string
	err := r.db.QueryRowContext(ctx, `SELECT hash, part, body FROM step_prompts WHERE hash = ?`, hash).
		Scan(&p.Hash, &part, &p.Body)
	if err == sql.ErrNoRows {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: step_prompts get: %w", err)
	}
	p.Part = persistence.StepPromptPart(part)
	return &p, nil
}

// PruneUnreferenced deletes every part no surviving outcome row points at.
func (r *StepPromptRepository) PruneUnreferenced(ctx context.Context) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("sqlite: step_prompts: no database handle")
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM step_prompts
		WHERE NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_system_hash = step_prompts.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_user_hash = step_prompts.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_tools_hash = step_prompts.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.input_hash = step_prompts.hash)
		  AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.result_hash = step_prompts.hash)`)
	if err != nil {
		return 0, fmt.Errorf("sqlite: step_prompts prune: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

var _ persistence.StepPromptRepository = (*StepPromptRepository)(nil)
