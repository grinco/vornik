// Package persistence — audit + observability interfaces.
//
// Three audit surfaces: ToolAudit (agent-container + companion tool calls), AdminAudit (operator actions on the admin surface), ChatAudit (per-dispatcher-turn record).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
)

// ToolAuditRepository persists tool invocation audit entries.
type ToolAuditRepository interface {
	// Log records a single tool invocation.
	Log(ctx context.Context, entry *ToolAuditEntry) error

	// List returns tool audit entries matching the filter.
	List(ctx context.Context, filter ToolAuditFilter) ([]*ToolAuditEntry, error)

	// CountByTool returns tool invocation counts grouped by tool name.
	CountByTool(ctx context.Context, executionID string) (map[string]int64, error)
}

// RecoveryEventRepository persists graceful-recovery markers — one row each
// time an execution reaches a terminal flagged Recovery:true (an intentional
// on_fail→recovery exit). Append-only; backs the recovery-trends view.
// See https://docs.vornik.io
type RecoveryEventRepository interface {
	// Record appends one recovery event.
	Record(ctx context.Context, e *RecoveryEvent) error
	// ListRecent returns recent recovery events (project="" = all), newest
	// first, bounded by limit.
	ListRecent(ctx context.Context, projectID string, limit int) ([]*RecoveryEvent, error)
}

// AdminAuditRepository persists daemon-level admin actions for the
// /ui/admin/audit surface. Distinct from ToolAuditRepository (tool
// invocations inside an agent workflow) — admin_audit rows are
// operator actions: config edits, MCP refreshes, danger-zone
// confirmations, etc. Every admin POST handler writes one row before
// returning so an operator's audit trail is durable even when the
// daemon crashes mid-mutation.
type AdminAuditRepository interface {
	// Insert writes one admin-audit row. Returns ErrDuplicateKey
	// when the supplied ID collides with an existing row; callers
	// generate IDs via persistence.GenerateID so collisions are
	// effectively impossible in practice.
	Insert(ctx context.Context, entry *AdminAuditEntry) error

	// List returns admin-audit rows matching the filter, newest first.
	// Implementations must bound the query with filter.PageSize > 0
	// — an unbounded query against this table is a footgun on busy
	// admin surfaces.
	List(ctx context.Context, filter AdminAuditFilter) ([]*AdminAuditEntry, error)
}

// ChatAuditRepository persists per-turn dispatcher activity for
// the `/ui/admin/chat-audit` operator surface. One row per inbound
// user message processed through the LLM tool loop — system prompt
// hash, model, tool calls, response excerpt, cost. Operators use it
// to answer "why did the bot do (or not do) X this turn?" without
// log grepping.
//
// SystemPromptHash points at chat_system_prompts (content-addressed)
// so the prompt body — typically 5-10 KB — is stored once per
// distinct hash regardless of how many turns referenced it.
type ChatAuditRepository interface {
	// Insert writes one chat-audit row. Returns ErrDuplicateKey when
	// the supplied ID collides with an existing row.
	Insert(ctx context.Context, entry *ChatAuditEntry) error

	// List returns chat-audit rows matching the filter, newest first.
	// Implementations must bound the query with filter.PageSize > 0.
	List(ctx context.Context, filter ChatAuditFilter) ([]*ChatAuditEntry, error)

	// GetByID fetches a single chat-audit row by its PK (the value a task's
	// ChatTurnID carries). Returns ErrNotFound when no row matches. Used by
	// the steering notifier to durably resolve a task's originating channel +
	// session across daemon restarts. Populates at least ID, ChatID, UserID,
	// ProjectID; other fields are best-effort.
	GetByID(ctx context.Context, id string) (*ChatAuditEntry, error)

	// SavePrompt stores a system prompt body keyed by its sha256 hex
	// digest. Idempotent: a second call with the same hash is a
	// no-op (the digest IS the identity check). Returns
	// ErrDuplicateKey only when two different bodies somehow hash to
	// the same value — operationally impossible but surfaced for
	// caller awareness.
	SavePrompt(ctx context.Context, hash, body string) error

	// GetPrompt fetches the body for a hash. Returns ErrNotFound
	// when no row matches.
	GetPrompt(ctx context.Context, hash string) (string, error)
}
