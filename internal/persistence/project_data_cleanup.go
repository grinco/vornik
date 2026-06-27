package persistence

import "context"

// ProjectDataStats summarises what a wipe touched. Counts are
// per-table best-effort — backends that can't report affected
// rows (sqlite mock paths, etc.) return -1 for that table rather
// than failing the cleanup. Audit + UI surfaces tolerate -1 by
// rendering "unknown" instead of a misleading zero.
type ProjectDataStats struct {
	// TablesCleared is the count of project-scoped tables the
	// deleter ran a DELETE against. Equal to len(ProjectDataTables)
	// on the happy path. Lower when an early-table delete failed
	// and the transaction rolled back.
	TablesCleared int

	// RowsDeleted aggregates affected-row counts across every
	// table when the backend reports them (postgres RowsAffected).
	// -1 means at least one DELETE returned an unknown count;
	// callers should treat the number as a lower bound or
	// "unknown".
	RowsDeleted int64
}

// ProjectDataDeleter wipes every row in every project-scoped
// table for one project ID. Implementations run the work in a
// single transaction so a mid-way failure rolls back cleanly —
// the archived-project sweeper retries on its next tick rather
// than fighting a partial state.
//
// Does NOT delete:
//
//   - admin_audit_log: preserves the historical record of who
//     archived/deleted what. The action row references the
//     project ID as Target so investigators can trace deletions
//     even after the rest is gone.
//   - cross_project_calls where this project is *caller*: the
//     callee may still want to see the inbound edge for their
//     own replay tree. The CPC row's caller_project becomes a
//     dangling reference; the replay builder already tolerates
//     that.
//
// Returns ProjectDataStats describing the wipe scope so the
// sweeper can write a precise audit row.
type ProjectDataDeleter interface {
	DeleteProjectData(ctx context.Context, projectID string) (ProjectDataStats, error)
}

// ProjectDataTables enumerates every table whose rows are
// scoped to a project_id column and which should be wiped when
// the project is deleted. Order matters only for FKs that don't
// cascade — we run children before parents to be defensive even
// though most child tables have ON DELETE CASCADE on a parent
// (tasks/executions/artifacts).
//
// Update this list when a new project-scoped table lands. The
// archive sweeper's test pins a representative subset; the rest
// follow by convention.
//
// Note: api_keys is intentionally included — once the project
// is gone its keys must stop authenticating too.
var ProjectDataTables = []string{
	// Trading lineage — fills cascade off orders so deleting
	// orders first keeps the FK contract tidy.
	"trading_fills",
	"trading_orders",
	"trading_positions_snapshots",
	"trading_safety_events",

	// Memory pipeline — child queues + DLQ first, then the
	// chunks they reference (project_memory_chunks has
	// CASCADE-from-chunk dependants, so deleting chunks
	// cascades to mention rows etc.).
	"memory_embed_queue",
	"memory_embed_dlq",
	"memory_eviction_audit",
	"project_memory_quarantine",
	"project_memory_chunks",
	"project_ingest_queue",

	// Knowledge graph — entities cascade to edges + mentions.
	"knowledge_entities",

	// Corpus epoch ledger.
	"corpus_rollbacks",
	"corpus_epochs_active",
	"corpus_embedding_versions_active",
	"corpus_epochs",

	// Audit + analytics — task-scoped tables cascade off
	// tasks; project-scoped ones go via project_id.
	"task_judge_verdicts",
	"task_post_mortems",
	"task_llm_usage",
	"execution_step_outcomes",
	"chat_audit_log",
	"intent_verdicts",
	"tool_audit_log",
	"webhook_events",
	"autonomy_evaluations",

	// Workflow self-healing (blackbox): triggers + per-class
	// override rows are both project-scoped (project_id NOT NULL).
	// Without these, deleting a project orphans its open healing
	// triggers — they linger in /ui/admin/blackbox/triggers/ for a
	// workflow whose project no longer exists, and a stale override
	// keeps muting/retuning detection for the dead project. Neither
	// table FK-cascades, so an explicit project_id pass is the only
	// thing that wipes them.
	"workflow_healing_triggers",
	"workflow_healing_overrides",

	// Documents + per-project gist cache.
	"extracted_documents",
	"project_gists",

	// Dispatcher reminders (B-12, 2026-05-28): pending reminders
	// tied to a deleted project would otherwise fire after the
	// project's gone — operator sees a phantom Telegram/email
	// reminder from a project they can't navigate to anymore.
	// Nullable project_id means reminders with NULL project_id
	// (operator-set without a project context) survive — those
	// aren't tied to any specific project's lifecycle anyway.
	"dispatcher_reminders",

	// project_wizard_sessions is intentionally NOT included:
	// the table is operator-scoped (committed_project_id is the
	// only project linkage, and uncommitted sessions have no
	// project linkage at all). A future iteration may add a
	// `DELETE WHERE committed_project_id = $1` pass; today the
	// session rows stay so the operator's wizard transcript
	// history outlives any single project's lifecycle.
	//
	// telegram_task_threads is intentionally NOT included: it
	// cascades via task_id ON DELETE CASCADE when `tasks` is
	// wiped below. Explicit DELETE would error on the missing
	// project_id column.

	// Artifacts row (the blob file is deleted out of band by
	// the sweeper's filesystem step).
	"artifacts",

	// Core tasks/executions last so the cascades into
	// task-scoped tables (task_messages, task_watchers,
	// execution_hints, etc.) run after their parent rows.
	"executions",
	"tasks",

	// API keys — scoped to the project.
	"api_keys",
}
