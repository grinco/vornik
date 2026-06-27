package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

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

// HardEvict permanently deletes chunkIDs from project_memory_chunks,
// cascading through memory_embed_queue + memory_embed_dlq +
// entity_mentions (all FK ON DELETE CASCADE) and nulling out
// project_memory_quarantine.released_chunk_id where it pointed at
// the evicted chunk. memory_retrieval_audit.chunk_ids is an array
// column with no FK, so historical retrieval rows retain the
// original chunk_id — correct: the audit trail should NOT pretend
// the chunk never existed.
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
func (r *Repository) HardEvict(ctx context.Context, projectID string, chunkIDs []string, reason, evictedBy string) ([]EvictionAuditRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repo: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory repo: project id required")
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

	// Snapshot the chunks we're about to delete so the audit row
	// gets denormalised values. SELECT ... FOR UPDATE locks the
	// rows against concurrent edits between this read and the
	// DELETE below — important because validation_status flips
	// (refuted, superseded) racing the eviction would otherwise
	// silently lose the snapshot.
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+1)
	args = append(args, projectID)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	snapshotQuery := fmt.Sprintf(`
		SELECT id, COALESCE(content_hash, ''), COALESCE(source_name, ''),
		       COALESCE(content_class, ''), COALESCE(producer_role, '')
		FROM project_memory_chunks
		WHERE project_id = $1
		  AND id IN (%s)
		FOR UPDATE
	`, strings.Join(placeholders, ","))
	rows, err := tx.QueryContext(ctx, snapshotQuery, args...)
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
	if len(audit) == 0 {
		// Nothing to delete — every ID was stale or wrong-project.
		// Commit the empty transaction to release the (empty) lock.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("memory repo: commit empty eviction: %w", err)
		}
		committed = true
		return nil, nil
	}

	// Write audit rows BEFORE the DELETE. If the audit insert fails
	// the chunk survives, and the operator can retry. (If we wrote
	// the audit AFTER, a panic between DELETE and INSERT would lose
	// the audit trail entirely.)
	for _, row := range audit {
		auditID := fmt.Sprintf("evict_%d_%s", time.Now().UnixNano(), row.ChunkID)
		const insertAudit = `
INSERT INTO memory_eviction_audit
    (id, project_id, chunk_id, content_hash, source_name,
     content_class, producer_role, reason, evicted_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`
		if _, err := tx.ExecContext(ctx, insertAudit,
			auditID, projectID, row.ChunkID, row.ContentHash, row.SourceName,
			row.ContentClass, row.ProducerRole, reason, evictedBy,
		); err != nil {
			return nil, fmt.Errorf("memory repo: insert eviction audit: %w", err)
		}
	}

	// Now delete the chunks. FK CASCADE handles memory_embed_queue,
	// memory_embed_dlq, entity_mentions; project_memory_quarantine
	// gets released_chunk_id nulled where it referenced these.
	auditedIDs := make([]string, 0, len(audit))
	for _, row := range audit {
		auditedIDs = append(auditedIDs, row.ChunkID)
	}
	delPlaceholders := make([]string, len(auditedIDs))
	delArgs := make([]any, 0, len(auditedIDs)+1)
	delArgs = append(delArgs, projectID)
	for i, id := range auditedIDs {
		delPlaceholders[i] = fmt.Sprintf("$%d", i+2)
		delArgs = append(delArgs, id)
	}
	deleteQuery := fmt.Sprintf(`
		DELETE FROM project_memory_chunks
		WHERE project_id = $1
		  AND id IN (%s)
	`, strings.Join(delPlaceholders, ","))
	if _, err := tx.ExecContext(ctx, deleteQuery, delArgs...); err != nil {
		return nil, fmt.Errorf("memory repo: delete chunks: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("memory repo: commit eviction: %w", err)
	}
	committed = true
	return audit, nil
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
