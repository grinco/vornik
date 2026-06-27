// Package persistence — memory ingest + audit interfaces.
//
// The five repos owning the RAG memory pipeline: project_ingest_queue (producer→worker handoff), memory_ingest_audit + memory_retrieval_audit (the LLD-22 audit trail), project_memory_quarantine (gate-failed chunks), corpus_epochs (snapshot/rollback machinery).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
	"time"
)

// IngestQueueRepository owns project_ingest_queue. Producers enqueue
// when they have an OUTPUT artifact ready for the memory pipeline;
// the IngestWorker drains and processes per-project batches. The
// queue's purpose is decoupling: producers don't block on chunking,
// embedding, or (Phase 2+) the rag-ingest swarm pipeline.
//
// Phase 1 acceptance: every memory ingest writes a queue row before
// the chunk; the worker drains the row and calls IngestText
// synchronously, bypassing the swarm. Phase 2 replaces the
// IngestText call with a rag-ingest task dispatch.
type IngestQueueRepository interface {
	// Enqueue inserts one queue row. ID is generated when empty.
	// EnqueuedAt defaults to NOW() server-side; client may override
	// for fixture tests.
	Enqueue(ctx context.Context, item *IngestQueueItem) error

	// ClaimBatch atomically transitions up to `limit` rows from
	// 'queued' → 'processing' for the given project, ordered by
	// priority then age. Uses FOR UPDATE SKIP LOCKED so multiple
	// workers (or a future per-project sharded worker) don't race.
	// Sets started_at and increments attempts. Returns the claimed
	// rows in the same order they'll be processed.
	ClaimBatch(ctx context.Context, projectID string, limit int) ([]*IngestQueueItem, error)

	// MarkDone transitions one item from 'processing' → 'done' and
	// stamps finished_at. Idempotent: succeeds even when the row
	// is already 'done' (caller may retry on transient ack failures).
	MarkDone(ctx context.Context, id string) error

	// MarkFailed transitions one item from 'processing' → 'failed'
	// when attempts has reached maxAttempts; otherwise transitions
	// back to 'queued' for another try, recording last_error.
	// Returns true when the item went to terminal 'failed' state.
	MarkFailed(ctx context.Context, id string, maxAttempts int, errorMsg string) (terminal bool, err error)

	// ProjectsWithQueued returns the distinct projects that have at
	// least one row in 'queued' state. Lets the worker scan only
	// projects that have work to do instead of every known project.
	ProjectsWithQueued(ctx context.Context, limit int) ([]string, error)

	// QueueDepth returns the current count of 'queued' + 'processing'
	// rows for a project. Powers the queue-depth gauge used by the
	// circuit-breaker alert.
	QueueDepth(ctx context.Context, projectID string) (int, error)

	// ResetStaleProcessing transitions any row whose state is
	// 'processing' and whose started_at is older than `olderThan` back
	// to 'queued', preserving the attempts counter and clearing
	// started_at. It is called on daemon startup to recover rows that
	// the previous incarnation claimed but never finished (crash mid-
	// batch, killed during graceful shutdown, etc.) — without this
	// sweep those rows would stay 'processing' indefinitely and the
	// queue would appear to have stalled depth that no worker will
	// touch. Returns the number of rows reset. Safe to run while a
	// live worker is processing: the age filter guarantees the sweep
	// only catches genuinely stuck rows, not freshly-claimed ones.
	ResetStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error)

	// CountStaleProcessing returns the count of rows currently in
	// 'processing' state with started_at older than `olderThan`. The
	// gauge powered by this is the post-fix observability for the
	// "stale processing rows" gap — a non-zero value indicates either
	// a worker is genuinely wedged on one item or the startup sweep
	// hasn't run since a crash. Cheap; safe to poll once per tick.
	CountStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error)
}

// MemoryIngestAuditRepository persists per-call ingest records for
// the companion-direct deposit path (LLD-22 backfill 2026-05-27).
// Agent deposits route through project_ingest_queue + producer_role,
// which already provides per-call traceability; companion-direct
// deposits bypass the queue and have no other audit record once
// rejected (chunks would never land; quarantine row only on
// quarantine; rejected calls had no trace at all). This repo
// captures one row per attempt regardless of decision.
//
// Append-only. Empty/nil callers (tests, deployments without the
// hook wired) skip writes — the rest of the daemon never depends
// on this repo being non-nil.
type MemoryIngestAuditRepository interface {
	// Record inserts one ingest-attempt row.
	Record(ctx context.Context, audit *MemoryIngestAudit) error

	// ListByProject returns recent ingest-audit rows for one project,
	// newest first, capped at limit. Powers the /ui/memory audit
	// dashboard's ingest-side panel and the SaaS-tier compliance
	// surface ("show every deposit by API key X this week").
	ListByProject(ctx context.Context, projectID string, limit int) ([]*MemoryIngestAudit, error)

	// List is the filter-aware variant powering B-16's
	// /ui/admin/memory-audit ingest panel. Rejects filter.PageSize
	// <= 0 to keep the page bounded.
	List(ctx context.Context, filter MemoryIngestAuditFilter) ([]*MemoryIngestAudit, error)
}

// MemoryRetrievalAuditRepository persists per-search records of which
// memory chunks were returned. Powers chunk-utility analytics: the
// auto-prune candidate list (chunks never retrieved) and the
// retrieval-success heuristic (chunks fed into successful steps).
//
// Empty/nil callers (e.g. tests, deployments without memory) skip
// writes — the rest of the daemon never depends on this repo being
// non-nil.
type MemoryRetrievalAuditRepository interface {
	// Record inserts one retrieval row.
	Record(ctx context.Context, audit *MemoryRetrievalAudit) error

	// FeedbackStats returns aggregated chunk-utility stats for one
	// project over a rolling window starting at `since`. Powers the
	// `vornikctl memory feedback` CLI surface.
	FeedbackStats(ctx context.Context, projectID string, since time.Time) (*MemoryFeedbackStats, error)

	// UnretrievedChunkIDs returns the IDs of chunks indexed for the
	// project that haven't appeared in any retrieval row since
	// `since`. Caller renders these as auto-prune candidates; the
	// repo doesn't perform the deletion.
	UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error)

	// List returns recent retrieval rows matching the filter, newest
	// first. Implementations must reject filter.PageSize <= 0 to
	// prevent unbounded scans — this is a hot operator surface on
	// projects with thousands of searches per day. Powers B-16's
	// /ui/admin/memory-audit retrieval panel.
	List(ctx context.Context, filter MemoryRetrievalAuditFilter) ([]*MemoryRetrievalAudit, error)
}

// MemoryQuarantineRepository persists chunks that ingest gates
// refused. DMARC-style — they aren't dropped silently; operators
// can inspect, release (after overriding the gate), or drop.
type MemoryQuarantineRepository interface {
	Insert(ctx context.Context, item *MemoryQuarantineItem) error
	ListPending(ctx context.Context, projectID string, limit int) ([]*MemoryQuarantineItem, error)
	Get(ctx context.Context, id string) (*MemoryQuarantineItem, error)
	MarkReleased(ctx context.Context, id, releasedChunkID string) error
	MarkDropped(ctx context.Context, id string) error
	CountByGate(ctx context.Context, projectID string) (map[string]int, error)
}

// CorpusEpochRepository owns the snapshot + rollback machinery
// (corpus_epochs, corpus_epochs_active, corpus_rollbacks). See
// https://docs.vornik.io §8 and, for the
// rollback × supersession restore semantics (migration 89),
// https://docs.vornik.io
type CorpusEpochRepository interface {
	CreateEpoch(ctx context.Context, e *CorpusEpoch) error
	CloseEpoch(ctx context.Context, epochID string, counts CorpusEpochCounts) error
	Activate(ctx context.Context, projectID, epochID, by, reason string) error
	// Deactivate removes an epoch from the active set AND stamps the
	// explicit-deactivation tombstone on corpus_epochs, so a later
	// rollback's re-activation pass cannot resurrect it. Activate
	// clears the tombstone.
	Deactivate(ctx context.Context, projectID, epochID, by string) error
	ListActive(ctx context.Context, projectID string) ([]string, error)
	ListEpochs(ctx context.Context, projectID string, limit int) ([]*CorpusEpoch, error)
	GetEpoch(ctx context.Context, epochID string) (*CorpusEpoch, error)
	// RollbackTo deactivates every epoch newer than the target,
	// re-activates the non-tombstoned epochs at or before it, and
	// restores chunks whose supersession was CAUSED by a
	// now-deactivated epoch (validation_status back to
	// pre_supersede_status). chunksRestored is the restore-pass count,
	// also recorded on the corpus_rollbacks audit row.
	RollbackTo(ctx context.Context, projectID, targetEpochID, triggeredBy, reason string) (deactivated, activated, chunksRestored int, err error)
	// CountRollbackRestorable previews the restore pass for a rollback
	// to targetEpochID: restorable = superseded chunks whose causing
	// epoch would be deactivated; nonRestorable = superseded chunks in
	// the project with no recorded provenance (pre-migration-89
	// history), surfaced so the gap is visible rather than silent.
	CountRollbackRestorable(ctx context.Context, projectID, targetEpochID string) (restorable, nonRestorable int, err error)
	ListRollbacks(ctx context.Context, projectID string, limit int) ([]*CorpusRollback, error)
}
