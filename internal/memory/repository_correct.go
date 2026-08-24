package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/graphsweep"
	"vornik.io/vornik/internal/persistence"
)

// ListChunkIDsByFailedProducer returns the IDs of every chunk in projectID
// whose producing EXECUTION ended unsuccessfully (FAILED or CANCELLED) — the
// retro-clean candidate set for the RAG-ingest producer-success gate (LLD
// 2026-07-12-rag-ingest-producer-success-gate §5). The join is
// chunks.artifact_id → artifacts.execution_id → executions.status, so a
// task's failed-execution chunks are selected while its successfully-retried
// (COMPLETED) execution's chunks are NOT. Chunks with an empty task_id
// (companion notes / uploaded docs) have no producer execution and are never
// selected. Always project-scoped (the IDOR guard). The caller feeds the
// result to HardEvict, which does the tx-safe cascade + per-chunk audit row.
func (r *Repository) ListChunkIDsByFailedProducer(ctx context.Context, projectID string) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory repo: project id required")
	}
	const q = `
SELECT c.id
FROM project_memory_chunks c
JOIN artifacts a ON c.artifact_id = a.id
JOIN executions e ON a.execution_id = e.id
WHERE c.project_id = $1
  AND c.task_id <> ''
  AND e.status IN ('FAILED', 'CANCELLED')`
	rows, err := r.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("memory repo: list failed-producer chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("memory repo: scan chunk id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory repo: iterate failed-producer chunks: %w", err)
	}
	return ids, nil
}

// operatorCorrectionRow is the narrow shape insertOperatorCorrection
// accepts. Kept private — the public surface (Corrector) builds the
// row from its own inputs; outside callers should go through
// Corrector.InsertCorrection rather than poking the repo directly.
type operatorCorrectionRow struct {
	ID          string
	ProjectID   string
	SourceName  string
	Content     string
	ContentHash string
	// RepoScope partitions the correction within the project's
	// RAG (migration 75). Empty = legacy NULL-scoped (matches
	// pre-fix behaviour: visible in lenient-scope searches,
	// invisible in strict-scope searches). Callers that know the
	// active scope should pass it so the correction surfaces under
	// the right repo bucket. Added 2026-05-29 to close the gap
	// where operator corrections were always NULL-scoped + thus
	// unreachable under strict-scope filtering.
	RepoScope string
}

// MarkRefutedByIDs flips validation_status to 'refuted' for every
// chunk in chunkIDs that currently lives under projectID. The
// project-scope filter is the IDOR guard: an attacker who guesses
// an ID from another project can't trip refutation on it. Returns
// the count of rows actually flipped — duplicate / already-refuted
// IDs collapse to zero on the second call (idempotent at the
// caller boundary).
func (r *Repository) MarkRefutedByIDs(ctx context.Context, projectID string, chunkIDs []string) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" || len(chunkIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+1)
	args = append(args, projectID)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2) // $1 is project_id
		args = append(args, id)
	}
	query := fmt.Sprintf(`
		UPDATE project_memory_chunks
		SET validation_status = 'refuted'
		WHERE project_id = $1
		  AND id IN (%s)
		  AND validation_status NOT IN ('refuted', 'superseded')
	`, strings.Join(placeholders, ","))
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mark refuted: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ChunkPreviewByID returns a chunk's source_name + content for a
// single (projectID, chunkID), scoped to the project (the IDOR guard:
// a chunk id from another project resolves to found=false here, so
// the by-id forget path can never read or touch it). found=false when
// no such chunk exists under projectID. Used by Corrector.ForgetByID
// to build a truthful "this is what I evicted" preview without a
// second round-trip. No validation_status filter — an operator can
// forget a chunk regardless of its current state (and re-forgetting an
// already-refuted chunk is a harmless no-op at MarkRefutedByIDs).
func (r *Repository) ChunkPreviewByID(ctx context.Context, projectID, chunkID string) (sourceName, content string, found bool, err error) {
	if r == nil || r.db == nil {
		return "", "", false, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" || chunkID == "" {
		return "", "", false, fmt.Errorf("memory repo: project id and chunk id required")
	}
	const q = `
		SELECT COALESCE(source_name, ''), COALESCE(content, '')
		FROM project_memory_chunks
		WHERE project_id = $1 AND id = $2`
	row := r.db.QueryRowContext(ctx, q, projectID, chunkID)
	switch err := row.Scan(&sourceName, &content); err {
	case nil:
		return sourceName, content, true, nil
	case sql.ErrNoRows:
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("memory repo: chunk preview by id: %w", err)
	}
}

// insertOperatorCorrection writes a new chunk carrying an
// operator's authoritative correction. The row lands as
// validation_status='verified' (so retrieval includes it),
// content_class='decision' (so role-based boosting treats it
// authoritatively), and producer_role='operator_correction' (so
// audit queries can find every operator-induced edit).
//
// content_hash is the dedup key on (project_id, content_hash);
// callers prepend a timestamp header so two corrections with the
// same body don't collide.
func (r *Repository) insertOperatorCorrection(ctx context.Context, row *operatorCorrectionRow) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("memory repo: not configured")
	}
	if row == nil || row.ID == "" || row.ProjectID == "" || row.Content == "" {
		return fmt.Errorf("memory repo: insertOperatorCorrection: missing required fields")
	}
	const q = `
INSERT INTO project_memory_chunks
    (id, project_id, source_name, chunk_index, content, content_hash,
     content_class, validation_status, producer_role, confidence,
     lifecycle_state, needs_graph_extraction, repo_scope)
VALUES ($1, $2, $3, 0, $4, $5,
        'decision', 'verified', 'operator_correction', 0.95,
        'published', TRUE, $6)`
	if _, err := r.db.ExecContext(ctx, q,
		row.ID, row.ProjectID, row.SourceName, row.Content, row.ContentHash,
		nullableString(row.RepoScope),
	); err != nil {
		return fmt.Errorf("insert operator correction: %w", err)
	}
	return nil
}

// ListUnverifiedChunks powers `vornikctl memory audit` — returns
// chunks that are still in the unverified / legacy validation
// states for operator review. Newest first; cap defaults to 100
// to keep the operator UI scannable.
type UnverifiedChunkRow struct {
	ID               string
	SourceName       string
	ContentTitle     string
	ContentClass     string
	ValidationStatus string
	ProducerRole     string
	Preview          string
	CreatedAt        string
}

// EvictionAuditRow is the per-row record HardEvict returns so the
// caller can surface "I evicted these chunks" back to the operator
// without a second DB round-trip. Mirrors the memory_eviction_audit
// schema's denormalised snapshot fields.
type EvictionAuditRow struct {
	ChunkID      string
	ContentHash  string
	SourceName   string
	ContentClass string
	ProducerRole string
}

// EvictionResult is what one hard eviction removed.
//
// The derived counts are here for the same reason they are on the Article 17
// erasure result: once these rows are gone, the report is the only evidence the
// eviction covered them. An eviction that listed chunks and said nothing about
// what those chunks had derived is how 3,795 knowledge-graph entities
// accumulated in production behind deletions reported as complete.
type EvictionResult struct {
	// Audit is one row per chunk actually deleted. May be shorter than the
	// requested set when ids were stale, wrong-project, or already evicted.
	Audit []EvictionAuditRow
	// Derived counts knowledge entities and edges removed with the chunks.
	Derived graphsweep.Counts
	// QuarantinedCopiesDeleted counts project_memory_quarantine rows removed —
	// the pre-ingest copy of the chunk's full text.
	QuarantinedCopiesDeleted int
	// EmbeddingCacheKeysDeleted counts embedding_cache rows removed: the vector
	// derived from the evicted text, keyed by that text's hash.
	EmbeddingCacheKeysDeleted int
	// RunID identifies the memory_eviction_runs row this eviction wrote — the
	// DURABLE record of everything above. The per-chunk tombstones account for
	// the chunks; until 2026-08-21 nothing recorded what those chunks derived.
	RunID string
}

// Count is how many chunks were evicted.
func (r *EvictionResult) Count() int {
	if r == nil {
		return 0
	}
	return len(r.Audit)
}

// HardEvict permanently deletes chunkIDs from project_memory_chunks,
// cascading through memory_embed_queue + memory_embed_dlq +
// entity_mentions (all FK ON DELETE CASCADE).
// memory_retrieval_audit.chunk_ids is an array
// column with no FK, so historical retrieval rows retain the
// original chunk_id — correct: the audit trail should NOT pretend
// the chunk never existed.
//
// IT ALSO REMOVES WHAT THE CHUNKS DERIVED, which it did not until 2026-08-21.
// A chunk delete cascades entity_mentions and stops: knowledge_entities and
// knowledge_edges have no foreign key to chunks, so the entities and edges
// built from an evicted chunk survived it. This command's own help names
// "GDPR / privacy-driven 'forget this' requests" as a use case, so an eviction
// that left the derived rows queryable was answering a privacy request with a
// partial deletion. The sweep is shared with the Article 17 path
// (internal/graphsweep) rather than reimplemented — the keep rule is subtle
// enough that two copies would agree until they didn't.
//
// The quarantined pre-ingest copy is DELETED, not detached. The foreign key on
// project_memory_quarantine.released_chunk_id is ON DELETE SET NULL, so before
// this change an eviction nulled the pointer and kept the text — and the text
// in that table is content an ingest gate REJECTED, which is
// disproportionately likely to be the sensitive kind. The FK rule stays SET
// NULL, because quarantine rows outlive their chunk by design; what changes is
// that a deletion meant to be permanent removes them explicitly. This happens
// BEFORE the chunk delete, since that delete would otherwise null the only
// handle onto those rows.
//
// One memory_eviction_audit row is written per evicted chunk,
// carrying a denormalised snapshot of the chunk's content_hash /
// source_name / content_class / producer_role plus the operator's
// reason + evicted_by identifier. The audit row IS the GDPR
// compliance hook — deletion without a record of the deletion
// would itself be non-compliant.
//
// Project-scope filter is the IDOR guard: a chunkID belonging to
// another project won't be touched. Returns the audit rows for
// the chunks that were actually deleted (may be shorter than
// chunkIDs if some IDs were stale / wrong-project / already
// evicted) along with the count of rows deleted.
//
// Single transaction. If the audit insert fails, the DELETE rolls
// back — the chunk row survives so the operator can retry. If the
// DELETE fails, the audit rows roll back too — no "we evicted X"
// audit ghost for a chunk that's still there.
func (r *Repository) HardEvict(ctx context.Context, projectID string, chunkIDs []string, reason, evictedBy string) (*EvictionResult, error) {
	if err := r.checkEvictArgs(projectID); err != nil {
		return nil, err
	}
	if len(chunkIDs) == 0 {
		return nil, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("memory repo: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// The run header is written FIRST because the tombstones foreign-key to it
	// and the FK is not deferred — a child naming a parent that does not exist
	// yet is rejected immediately. Its outcome counts are filled in at the end,
	// when they are known; both statements share this transaction, so a failure
	// leaves neither.
	runID := persistence.GenerateID("evrun")
	if err := openEvictionRun(ctx, tx, runID, projectID, len(chunkIDs), reason, evictedBy); err != nil {
		return nil, err
	}

	audit, err := snapshotChunksForEviction(ctx, tx, projectID, chunkIDs)
	if err != nil {
		return nil, err
	}
	if len(audit) == 0 {
		// Nothing to delete — every ID was stale or wrong-project. Commit
		// anyway: the transaction releases its (empty) lock, and the run header
		// written above is KEPT deliberately. An operator asking to erase ids
		// that no longer exist is still an action on personal data worth
		// recording, and the row reads truthfully — chunks_requested set, every
		// outcome count zero.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("memory repo: commit empty eviction: %w", err)
		}
		committed = true
		return &EvictionResult{}, nil
	}

	if err := writeEvictionAudits(ctx, tx, projectID, runID, audit, reason, evictedBy); err != nil {
		return nil, err
	}

	auditedIDs := make([]string, 0, len(audit))
	for _, row := range audit {
		auditedIDs = append(auditedIDs, row.ChunkID)
	}

	capturedEntities, cacheKeys, quarantined, err :=
		collectAndPurgePreChunkDelete(ctx, tx, projectID, auditedIDs)
	if err != nil {
		return nil, err
	}

	if err := deleteEvictedChunks(ctx, tx, projectID, auditedIDs); err != nil {
		return nil, err
	}

	if err := deleteCachedVectors(ctx, tx, cacheKeys); err != nil {
		return nil, err
	}

	// Derived rows last, so the keep rule is evaluated against the state AFTER
	// the chunks are gone. Same transaction: a partial eviction that removed
	// chunks and left their entities is the condition being fixed, so it must
	// not be reachable through a failure either.
	derived, err := graphsweep.Sweep(ctx, tx, auditedIDs, capturedEntities)
	if err != nil {
		return nil, fmt.Errorf("memory repo: %w", err)
	}

	res := &EvictionResult{
		Audit:                     audit,
		Derived:                   derived,
		QuarantinedCopiesDeleted:  quarantined,
		EmbeddingCacheKeysDeleted: len(cacheKeys),
		RunID:                     runID,
	}
	if err := closeEvictionRun(ctx, tx, runID, res); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("memory repo: commit eviction: %w", err)
	}
	committed = true
	return res, nil
}

// snapshotChunksForEviction reads the denormalised values the audit rows carry,
// and LOCKS the chunks against concurrent edits between this read and the
// delete — a validation_status flip (refuted, superseded) racing the eviction
// would otherwise silently lose the snapshot.
//
// ORDER BY id is not cosmetic. Two evictions whose chunk sets overlap would
// otherwise lock those rows in whatever order the IN-list gave them and could
// deadlock on each other; ordering makes them queue. The sweep orders its
// entity lock for the same reason, and both paths take chunks before entities,
// so the two tables are always acquired in one direction.
//
// Rows that come back are the ones that exist AND belong to projectID: the
// project filter is the IDOR guard, so a chunk id from another project is never
// touched. The returned slice may therefore be shorter than chunkIDs.
func snapshotChunksForEviction(
	ctx context.Context, tx *sql.Tx, projectID string, chunkIDs []string,
) ([]EvictionAuditRow, error) {
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+1)
	args = append(args, projectID)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, COALESCE(content_hash, ''), COALESCE(source_name, ''),
		       COALESCE(content_class, ''), COALESCE(producer_role, '')
		FROM project_memory_chunks
		WHERE project_id = $1
		  AND id IN (%s)
		ORDER BY id
		FOR UPDATE
	`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil, fmt.Errorf("memory repo: snapshot chunks: %w", err)
	}
	var audit []EvictionAuditRow
	for rows.Next() {
		var row EvictionAuditRow
		if err := rows.Scan(&row.ChunkID, &row.ContentHash, &row.SourceName,
			&row.ContentClass, &row.ProducerRole); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("memory repo: scan chunk snapshot: %w", err)
		}
		audit = append(audit, row)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("memory repo: close snapshot rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory repo: snapshot iteration: %w", err)
	}
	return audit, nil
}

// evictionCacheKeys collects the embedding_cache keys for the chunks about to
// be evicted, BEFORE they go — the key is the CONTEXTUALISED embed input's
// hash, which is unreadable once the row is gone.
//
// The cached vector is derived from the very text the operator asked to
// destroy, and nothing else removes it: the retention sweeper prunes
// embedding_cache by last_hit_at age and only when
// retention.embedding_cache_days is set, which is cold-entry pruning, not
// deletion. DeleteByArtifact and DeleteByExtractedDocument have cleaned it
// since slice 5c; eviction is the third path and was not wired to it, so a
// deletion documented as permanent left the vector behind.
func evictionCacheKeys(
	ctx context.Context, tx *sql.Tx, projectID string, chunkIDs []string,
) ([]string, error) {
	keys, err := chunkCacheKeys(ctx, tx, `
		SELECT embed_input_hash, source_name, content, content_hash
		FROM project_memory_chunks
		WHERE project_id = $1 AND id = ANY($2)`, projectID, pq.Array(chunkIDs))
	if err != nil {
		return nil, fmt.Errorf("memory repo: read chunk cache keys for eviction: %w", err)
	}
	return keys, nil
}

// deleteCachedVectors removes the collected embedding_cache rows, in the same
// transaction as the chunk delete: the chunk and its vector go together or
// neither goes.
func deleteCachedVectors(ctx context.Context, tx *sql.Tx, keys []string) error {
	for _, h := range keys {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embedding_cache WHERE content_hash = $1`, h); err != nil {
			return fmt.Errorf("memory repo: evict embedding cache: %w", err)
		}
	}
	return nil
}

// checkEvictArgs refuses an eviction that cannot be scoped. The project filter
// is the IDOR guard, so an absent project id would widen the delete, not narrow
// it.
func (r *Repository) checkEvictArgs(projectID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return fmt.Errorf("memory repo: project id required")
	}
	return nil
}

// collectAndPurgePreChunkDelete does the three things that MUST happen while
// the chunks still exist: capture the entities they mention (entity_mentions
// cascades with the chunk, so afterwards nothing can say which entities this
// eviction was responsible for), read the embedding-cache keys (the key is the
// contextualised embed input's hash, unreadable once the row is gone), and
// delete the quarantined copies (whose only pointer the chunk delete nulls).
//
// Grouped because they share that one constraint. Anything moved out of here to
// after the chunk delete stops working SILENTLY rather than failing, which is
// how each of them came to be missing in the first place.
func collectAndPurgePreChunkDelete(
	ctx context.Context, tx *sql.Tx, projectID string, chunkIDs []string,
) (entities, cacheKeys []string, quarantined int, err error) {
	entities, err = graphsweep.CaptureEntities(ctx, tx, chunkIDs)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("memory repo: %w", err)
	}
	cacheKeys, err = evictionCacheKeys(ctx, tx, projectID, chunkIDs)
	if err != nil {
		return nil, nil, 0, err
	}
	quarantined, err = graphsweep.DeleteQuarantinedForChunks(ctx, tx, chunkIDs)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("memory repo: %w", err)
	}
	return entities, cacheKeys, quarantined, nil
}

// openEvictionRun writes the run header at the START of an eviction, because
// the tombstones foreign-key to it. Outcome counts land in closeEvictionRun.
func openEvictionRun(
	ctx context.Context, tx *sql.Tx, runID, projectID string,
	requested int, reason, evictedBy string,
) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_eviction_runs
    (id, project_id, chunks_requested, reason, evicted_by)
VALUES ($1, $2, $3, $4, $5)
`, runID, projectID, requested, reason, evictedBy); err != nil {
		return fmt.Errorf("memory repo: open eviction run: %w", err)
	}
	return nil
}

// closeEvictionRun records what the operation actually removed, including what
// it removed BEYOND the chunks.
//
// In the SAME transaction as the deletes, and that is the point: a deletion of
// personal data with no record of the deletion is itself non-compliant, and a
// record of a deletion that did not happen is a false claim. Neither is
// reachable if they commit together.
//
// A run header rather than columns on each tombstone, because the derived sweep
// runs once over the union of the evicted chunks — repeating a per-operation
// count on per-chunk rows would be durable and wrong, reporting the derived
// rows twice when summed.
func closeEvictionRun(ctx context.Context, tx *sql.Tx, runID string, res *EvictionResult) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_eviction_runs
SET chunks_evicted             = $2,
    graph_entities_deleted     = $3,
    graph_edges_deleted        = $4,
    quarantined_copies_deleted = $5,
    cached_embeddings_deleted  = $6
WHERE id = $1
`, runID, len(res.Audit), res.Derived.Entities, res.Derived.Edges,
		res.QuarantinedCopiesDeleted, res.EmbeddingCacheKeysDeleted); err != nil {
		return fmt.Errorf("memory repo: close eviction run: %w", err)
	}
	return nil
}

// deleteEvictedChunks removes the chunks themselves. FK CASCADE handles
// memory_embed_queue, memory_embed_dlq and entity_mentions; the caller has
// already captured what those mentions pointed at, because they do not survive
// this statement.
func deleteEvictedChunks(
	ctx context.Context, tx *sql.Tx, projectID string, chunkIDs []string,
) error {
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+1)
	args = append(args, projectID)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM project_memory_chunks
		WHERE project_id = $1
		  AND id IN (%s)
	`, strings.Join(placeholders, ",")), args...); err != nil {
		return fmt.Errorf("memory repo: delete chunks: %w", err)
	}
	return nil
}

// writeEvictionAudits records the tombstones BEFORE the delete. If the audit
// insert fails the chunk survives and the operator can retry; writing the audit
// afterwards would let a crash between DELETE and INSERT lose the trail
// entirely, and a deletion with no record of the deletion is itself
// non-compliant.
func writeEvictionAudits(
	ctx context.Context, tx *sql.Tx, projectID, runID string,
	audit []EvictionAuditRow, reason, evictedBy string,
) error {
	const insertAudit = `
INSERT INTO memory_eviction_audit
    (id, project_id, chunk_id, content_hash, source_name,
     content_class, producer_role, reason, evicted_by, run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`
	for _, row := range audit {
		auditID := fmt.Sprintf("evict_%d_%s", time.Now().UnixNano(), row.ChunkID)
		if _, err := tx.ExecContext(ctx, insertAudit,
			auditID, projectID, row.ChunkID, row.ContentHash, row.SourceName,
			row.ContentClass, row.ProducerRole, reason, evictedBy, runID,
		); err != nil {
			return fmt.Errorf("memory repo: insert eviction audit: %w", err)
		}
	}
	return nil
}

// EvictionAuditEntry mirrors a single memory_eviction_audit row for
// the UI listing surface. Denormalised snapshot fields survive the
// chunk's deletion (the schema is intentionally FK-free against
// project_memory_chunks for exactly this reason).
type EvictionAuditEntry struct {
	ID           string
	ChunkID      string
	ContentHash  string
	SourceName   string
	ContentClass string
	ProducerRole string
	Reason       string
	EvictedBy    string
	EvictedAt    string
}

// ListEvictionAudits returns recent eviction tombstones for a
// project, newest first. Powers the UI compliance-audit panel that
// pairs with the Corrector.HardEvict CLI path. Limit defaults to
// 100 and caps at 500 so dumps stay readable in the terminal /
// browser table.
func (r *Repository) ListEvictionAudits(ctx context.Context, projectID string, limit int) ([]EvictionAuditEntry, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory repo: project id required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, chunk_id, content_hash, source_name,
		       content_class, producer_role, reason, evicted_by,
		       to_char(evicted_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS UTC')
		FROM memory_eviction_audit
		WHERE project_id = $1
		ORDER BY evicted_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list eviction audits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EvictionAuditEntry
	for rows.Next() {
		var e EvictionAuditEntry
		if err := rows.Scan(&e.ID, &e.ChunkID, &e.ContentHash, &e.SourceName,
			&e.ContentClass, &e.ProducerRole, &e.Reason, &e.EvictedBy, &e.EvictedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListUnverifiedChunks returns chunks awaiting validation review.
// Filters: project required; status defaults to ('unverified',
// 'legacy') — pass an explicit status slice to narrow further.
// Limit defaults to 100, capped at 500 so audit dumps stay
// scannable in a terminal.
func (r *Repository) ListUnverifiedChunks(ctx context.Context, projectID string, statuses []string, limit int) ([]UnverifiedChunkRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory repo: project id required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if len(statuses) == 0 {
		statuses = []string{"unverified", "legacy"}
	}
	// IN-list builder. $1 is project_id; $2..$N are statuses.
	args := make([]any, 0, len(statuses)+2)
	args = append(args, projectID)
	statusPlace := make([]string, len(statuses))
	for i, s := range statuses {
		statusPlace[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, s)
	}
	args = append(args, limit)
	limitPlace := fmt.Sprintf("$%d", len(args))
	query := fmt.Sprintf(`
		SELECT id, source_name,
		       COALESCE(content_title, ''), content_class, validation_status,
		       COALESCE(producer_role, ''),
		       substring(content, 1, 200),
		       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS UTC')
		FROM project_memory_chunks
		WHERE project_id = $1
		  AND validation_status IN (%s)
		ORDER BY created_at DESC
		LIMIT %s
	`, strings.Join(statusPlace, ","), limitPlace)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list unverified: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UnverifiedChunkRow
	for rows.Next() {
		var c UnverifiedChunkRow
		if err := rows.Scan(&c.ID, &c.SourceName, &c.ContentTitle, &c.ContentClass,
			&c.ValidationStatus, &c.ProducerRole, &c.Preview, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChunkIDsByScope returns the IDs of every chunk in a project under a
// given repo_scope — the resolver behind `vornikctl memory evict
// --scope`. scopeIsNull selects the UNTAGGED bucket (repo_scope IS
// NULL — the pre-migration-75 leak surface) and ignores scope;
// otherwise it matches repo_scope = scope exactly. The caller feeds
// the result to HardEvict, which does the tx-safe cascade + per-chunk
// audit tombstone. project_id is always bound (the IDOR guard).
func (r *Repository) ChunkIDsByScope(ctx context.Context, projectID, scope string, scopeIsNull bool) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory repo: project id required")
	}
	q := `SELECT id FROM project_memory_chunks WHERE project_id = $1 AND repo_scope IS NULL`
	args := []any{projectID}
	if !scopeIsNull {
		q = `SELECT id FROM project_memory_chunks WHERE project_id = $1 AND repo_scope = $2`
		args = append(args, scope)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory repo: chunk ids by scope: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
