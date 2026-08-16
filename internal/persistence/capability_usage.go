package persistence

import (
	"context"
	"time"
)

// CapabilityUsage is one capability's usage within one project.
//
// It backs the adoption leaderboard's competitive axis. The unit of competition
// is a PROJECT — a PoC team — and the score that drives the right behaviour is
// BREADTH: how many distinct capabilities a team has exercised, not how much
// volume it pushed through one of them. Ranking on volume rewards hammering a
// single feature; ranking on breadth rewards trying the next one, which is what
// a proof-of-concept is for and what an enablement session can act on.
//
// The zero rows matter as much as the large ones. A capability with no usage in
// any project is a feature the customer has not discovered; a capability used by
// one team and no others is precisely the enablement session worth running.
type CapabilityUsage struct {
	// Key is the stable capability id (never renamed once shipped — the UI and
	// any saved comparison key on it).
	Key string
	// ProjectID is empty for capabilities whose signal is not project-scoped;
	// those are reported instance-wide rather than dropped.
	ProjectID string
	Count     int64
	LastUsed  *time.Time
}

// CapabilityUsageRepository reports which product capabilities are being
// exercised, per project, over a window.
type CapabilityUsageRepository interface {
	// Usage returns one row per (capability, project) with a non-zero count,
	// plus a zero-count row for every catalogued capability that no project
	// used at all — the absent ones are the point, so they cannot be left out
	// by an inner join.
	Usage(ctx context.Context, since time.Time) ([]CapabilityUsage, error)
}

// CapabilitySignal maps a catalogued capability onto the table that evidences
// it. One row per capability, deliberately explicit rather than derived: a
// customer-facing name has to be chosen by a person, and a table appearing in
// the schema is not the same as a capability a customer would recognise.
//
// tsCol is per-table and must be verified against the schema rather than
// assumed: four of these tables do not have created_at (extracted_at,
// recorded_at, reserved_at, opened_at), and guessing produced a UNION that
// failed at query time rather than at review time.
//
// projectCol is empty when the signal is not project-scoped — those report
// instance-wide rather than being dropped, because "used, but we cannot say by
// whom" is still better information than silence.
type CapabilitySignal struct {
	Key        string
	Table      string
	TsCol      string
	ProjectCol string
	Where      string
}

// CapabilitySignals is the shipped catalogue, shared by every backend so two
// implementations cannot drift on what a capability is. Keys are stable once
// released — the UI and any saved comparison key on them.
//
// Zero-usage entries are the reason this exists: they are the enablement list.
// Anything whose absence is GOOD (security incidents, dead-letter queues) is
// deliberately NOT catalogued — presenting an empty incident table as an
// unexplored feature would invert its meaning.
// CapabilitySignals is the shipped catalogue, shared by every backend so the
// two implementations cannot drift on what a capability even is.
//
// Keys are stable once released: the UI and any saved comparison key on them.
var CapabilitySignals = []CapabilitySignal{
	// Core
	{Key: "tasks", Table: "tasks", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "tool_use", Table: "tool_audit_log", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "artifacts", Table: "artifacts", TsCol: "created_at", ProjectCol: "project_id"},
	// Knowledge
	{Key: "memory_search", Table: "memory_retrieval_audit", TsCol: "retrieved_at", ProjectCol: "project_id"},
	{Key: "memory_deposit", Table: "memory_ingest_audit", TsCol: "ingested_at", ProjectCol: "project_id"},
	{Key: "knowledge_graph", Table: "knowledge_entities", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "doc_extraction", Table: "extracted_documents", TsCol: "extracted_at", ProjectCol: "project_id"},
	// Channels
	{Key: "chat", Table: "chat_audit_log", TsCol: "ts", ProjectCol: "project_id"},
	{Key: "github", Table: "webhook_events", TsCol: "created_at", ProjectCol: "project_id", Where: "source = 'github'"},
	{Key: "companion", Table: "memory_retrieval_audit", TsCol: "retrieved_at", ProjectCol: "project_id", Where: "actor_kind LIKE 'companion:%'"},
	// Automation
	{Key: "autonomy", Table: "autonomy_evaluations", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "scheduled", Table: "tasks", TsCol: "created_at", ProjectCol: "project_id", Where: "creation_source = 'SCHEDULED'"},
	{Key: "a2a_inbound", Table: "tasks", TsCol: "created_at", ProjectCol: "project_id", Where: "creation_source = 'A2A'"},
	{Key: "webhooks", Table: "webhook_events", TsCol: "created_at", ProjectCol: "project_id"},
	// Advanced
	{Key: "instincts", Table: "instincts", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "control_plane", Table: "control_plane_proposals", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "quality_judging", Table: "task_judge_verdicts", TsCol: "recorded_at", ProjectCol: "project_id"},
	{Key: "budget_governance", Table: "budget_reservations", TsCol: "reserved_at", ProjectCol: "project_id"},
	{Key: "reminders", Table: "dispatcher_reminders", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "web_write", Table: "web_write_actions", TsCol: "created_at", ProjectCol: "project_id"},
	{Key: "fixit", Table: "fixit_sessions", TsCol: "created_at", ProjectCol: "project_id"},
	// Not project-scoped — reported instance-wide.
	{Key: "cross_project", Table: "cross_project_calls", TsCol: "created_at"},
	{Key: "project_spawn", Table: "project_spawns", TsCol: "created_at"},
	{Key: "a2a_push", Table: "a2a_push_configs", TsCol: "created_at"},
	{Key: "gdpr_dsr", Table: "data_subject_requests", TsCol: "opened_at"},
}
