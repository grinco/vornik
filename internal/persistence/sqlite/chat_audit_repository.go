package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ChatAuditRepository is the SQLite parity for the postgres
// ChatAuditRepository. Same shape; DOUBLE PRECISION drops to REAL,
// TIMESTAMPTZ drops to TEXT (RFC3339-stamped via sqliteTime).
type ChatAuditRepository struct {
	db DBTX
}

func NewChatAuditRepository(db DBTX) *ChatAuditRepository {
	return &ChatAuditRepository{db: db}
}

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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, sqliteTime(ts), entry.ChatID, entry.UserID, entry.ProjectID,
		entry.RoleUsed, entry.Model, entry.SystemPromptHash,
		entry.UserMessage, toolCallsJSON, entry.Response,
		entry.Iterations, entry.DurationMs, entry.CostUSD,
		entry.HallucinationSignalsJSON,
	)
	return err
}

// GetByID fetches one row by PK. Returns persistence.ErrNotFound when absent.
func (r *ChatAuditRepository) GetByID(ctx context.Context, id string) (*persistence.ChatAuditEntry, error) {
	if id == "" {
		return nil, persistence.ErrNotFound
	}
	e := &persistence.ChatAuditEntry{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, chat_id, user_id, project_id, role_used, model
		FROM chat_audit_log WHERE id = ?`, id,
	).Scan(&e.ID, &e.ChatID, &e.UserID, &e.ProjectID, &e.RoleUsed, &e.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persistence.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

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
	if filter.ChatID != "" {
		b.WriteString(" AND chat_id = ?")
		args = append(args, filter.ChatID)
	}
	if filter.ProjectID != "" {
		b.WriteString(" AND project_id = ?")
		args = append(args, filter.ProjectID)
	}
	if !filter.Since.IsZero() {
		b.WriteString(" AND ts >= ?")
		args = append(args, sqliteTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		b.WriteString(" AND ts <= ?")
		args = append(args, sqliteTime(filter.Until))
	}
	b.WriteString(" ORDER BY ts DESC LIMIT ? OFFSET ?")
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*persistence.ChatAuditEntry, 0, filter.PageSize)
	for rows.Next() {
		var e persistence.ChatAuditEntry
		var tsText string
		if err := rows.Scan(
			&e.ID, &tsText, &e.ChatID, &e.UserID, &e.ProjectID,
			&e.RoleUsed, &e.Model, &e.SystemPromptHash,
			&e.UserMessage, &e.ToolCallsJSON, &e.Response,
			&e.Iterations, &e.DurationMs, &e.CostUSD,
			&e.HallucinationSignalsJSON,
		); err != nil {
			return nil, fmt.Errorf("chat_audit: scan: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339Nano, tsText); perr == nil {
			e.Timestamp = t
		} else if t, perr := time.Parse(time.RFC3339, tsText); perr == nil {
			e.Timestamp = t
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat_audit: rows: %w", err)
	}
	return out, nil
}

func (r *ChatAuditRepository) SavePrompt(ctx context.Context, hash, body string) error {
	if hash == "" {
		return fmt.Errorf("chat_audit: empty hash")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO chat_system_prompts (hash, body, created_at)
		VALUES (?, ?, ?)`,
		hash, body, sqliteTime(time.Now().UTC()),
	)
	return err
}

func (r *ChatAuditRepository) GetPrompt(ctx context.Context, hash string) (string, error) {
	var body string
	err := r.db.QueryRowContext(ctx, `SELECT body FROM chat_system_prompts WHERE hash = ?`, hash).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", persistence.ErrNotFound
	}
	return body, err
}
