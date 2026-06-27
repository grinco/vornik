package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ChatAuditRepository implements persistence.ChatAuditRepository
// against PostgreSQL. Mirrors AdminAuditRepository's shape — the
// two surfaces (operator config-change audit vs per-turn chat
// audit) are intentionally parallel so the admin UI's filter +
// drill-down patterns transfer.
//
// chat_system_prompts is content-addressed: SavePrompt is
// idempotent on the hash key, GetPrompt looks up the body. The
// admin UI's chat-audit detail page renders the full prompt by
// joining on system_prompt_hash.
type ChatAuditRepository struct {
	db DBTX
}

// NewChatAuditRepository constructs a ChatAuditRepository over db.
func NewChatAuditRepository(db DBTX) *ChatAuditRepository {
	return &ChatAuditRepository{db: db}
}

// Insert writes one chat-audit row. Timestamp defaults to NOW()
// when the caller leaves entry.Timestamp zero.
func (r *ChatAuditRepository) Insert(ctx context.Context, entry *persistence.ChatAuditEntry) error {
	if entry == nil {
		return fmt.Errorf("chat_audit: nil entry")
	}
	if entry.ID == "" {
		entry.ID = persistence.GenerateID("chat")
	}
	ts := entry.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	toolCallsJSON := entry.ToolCallsJSON
	if toolCallsJSON == "" {
		toolCallsJSON = "[]"
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO chat_audit_log (
			id, ts, chat_id, user_id, project_id, role_used, model,
			system_prompt_hash, user_message, tool_calls_json,
			response, iterations, duration_ms, cost_usd,
			hallucination_signals_json
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		entry.ID, ts, entry.ChatID, entry.UserID, entry.ProjectID,
		entry.RoleUsed, entry.Model, entry.SystemPromptHash,
		entry.UserMessage, toolCallsJSON, entry.Response,
		entry.Iterations, entry.DurationMs, entry.CostUSD,
		entry.HallucinationSignalsJSON,
	)
	return mapDBError(err)
}

// GetByID fetches one row by PK. Returns persistence.ErrNotFound when absent.
func (r *ChatAuditRepository) GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error) {
	if id == "" {
		return nil, persistence.ErrNotFound
	}
	e := &persistence.ChatAuditEntry{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, ts, chat_id, user_id, project_id, role_used, model
		FROM chat_audit_log WHERE id = $1`, id,
	).Scan(&e.ID, &e.Timestamp, &e.ChatID, &e.UserID, &e.ProjectID, &e.RoleUsed, &e.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return e, nil
}

// List returns rows matching the filter, newest first.
func (r *ChatAuditRepository) List(ctx context.Context, filter persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("chat_audit: PageSize must be > 0")
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, ts, chat_id, user_id, project_id, role_used, model,
		       system_prompt_hash, user_message, tool_calls_json,
		       response, iterations, duration_ms, cost_usd,
		       COALESCE(hallucination_signals_json, '')
		FROM chat_audit_log WHERE 1=1`)
	args := make([]any, 0, 6)
	pos := 1
	if filter.ChatID != "" {
		fmt.Fprintf(&b, " AND chat_id = $%d", pos)
		args = append(args, filter.ChatID)
		pos++
	}
	if filter.ProjectID != "" {
		fmt.Fprintf(&b, " AND project_id = $%d", pos)
		args = append(args, filter.ProjectID)
		pos++
	}
	if !filter.Since.IsZero() {
		fmt.Fprintf(&b, " AND ts >= $%d", pos)
		args = append(args, filter.Since)
		pos++
	}
	if !filter.Until.IsZero() {
		fmt.Fprintf(&b, " AND ts <= $%d", pos)
		args = append(args, filter.Until)
		pos++
	}
	fmt.Fprintf(&b, " ORDER BY ts DESC LIMIT $%d OFFSET $%d", pos, pos+1)
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ChatAuditEntry, 0, filter.PageSize)
	for rows.Next() {
		var e persistence.ChatAuditEntry
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.ChatID, &e.UserID, &e.ProjectID,
			&e.RoleUsed, &e.Model, &e.SystemPromptHash,
			&e.UserMessage, &e.ToolCallsJSON, &e.Response,
			&e.Iterations, &e.DurationMs, &e.CostUSD,
			&e.HallucinationSignalsJSON,
		); err != nil {
			return nil, fmt.Errorf("chat_audit: scan: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat_audit: rows: %w", err)
	}
	return out, nil
}

// SavePrompt stores a system prompt body keyed by its sha256 hex
// digest. ON CONFLICT DO NOTHING is the idempotent-by-hash
// contract — the digest IS the identity.
func (r *ChatAuditRepository) SavePrompt(ctx context.Context, hash, body string) error {
	if hash == "" {
		return fmt.Errorf("chat_audit: empty hash")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO chat_system_prompts (hash, body, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (hash) DO NOTHING`,
		hash, body, time.Now().UTC(),
	)
	return mapDBError(err)
}

// GetPrompt returns the body for the given hash, or
// persistence.ErrNotFound when the row is absent.
func (r *ChatAuditRepository) GetPrompt(ctx context.Context, hash string) (string, error) {
	var body string
	err := r.db.QueryRowContext(ctx, `SELECT body FROM chat_system_prompts WHERE hash = $1`, hash).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", persistence.ErrNotFound
	}
	if err != nil {
		return "", mapDBError(err)
	}
	return body, nil
}
