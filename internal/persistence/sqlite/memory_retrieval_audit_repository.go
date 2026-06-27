package sqlite

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemoryRetrievalAuditRepository persists per-search records of which
// memory chunks were returned. Backfilled with full FeedbackStats +
// UnretrievedChunkIDs once project_memory_chunks landed in round 3.
type MemoryRetrievalAuditRepository struct {
	db DBTX
}

func NewMemoryRetrievalAuditRepository(db DBTX) *MemoryRetrievalAuditRepository {
	return &MemoryRetrievalAuditRepository{db: db}
}

// Record inserts one retrieval row.
func (r *MemoryRetrievalAuditRepository) Record(ctx context.Context, audit *persistence.MemoryRetrievalAudit) error {
	if audit == nil {
		return fmt.Errorf("MemoryRetrievalAuditRepository.Record: audit is nil")
	}
	if audit.ID == "" {
		audit.ID = persistence.GenerateID("retr")
	}
	if audit.RetrievedAt.IsZero() {
		audit.RetrievedAt = time.Now().UTC()
	}
	chunks := audit.ChunkIDs
	if chunks == nil {
		chunks = []string{}
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO memory_retrieval_audit (
			id, project_id, task_id, execution_id, step_id, role,
			query, chunk_ids, retrieved_at,
			actor_kind, actor_id, repo_scope
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.ID, audit.ProjectID, audit.TaskID, audit.ExecutionID, audit.StepID, audit.Role,
		audit.Query, sqliteStringArray(chunks), sqliteTime(audit.RetrievedAt),
		audit.ActorKind, audit.ActorID, audit.RepoScope,
	)
	return err
}

// FeedbackStats joins project_memory_chunks with memory_retrieval_audit
// to compute chunk-utility stats over a window.
//
// Implementation note: Postgres's `unnest(chunk_ids)` becomes
// SQLite's `json_each(chunk_ids)` — both unpack a JSON array of
// chunk IDs into one row per element. json_each is a table-valued
// function in SQLite and joins like a regular table.
func (r *MemoryRetrievalAuditRepository) FeedbackStats(ctx context.Context, projectID string, since time.Time) (*persistence.MemoryFeedbackStats, error) {
	if projectID == "" {
		return nil, fmt.Errorf("FeedbackStats: projectID is required")
	}
	out := &persistence.MemoryFeedbackStats{}

	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id = ?`,
		projectID,
	).Scan(&out.TotalChunks); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_retrieval_audit
		 WHERE project_id = ? AND retrieved_at >= ?`,
		projectID, sqliteTime(since),
	).Scan(&out.TotalSearches); err != nil {
		return nil, err
	}
	// json_each unpacks the chunk_ids JSON array; DISTINCT collapses
	// duplicates returned across multiple searches.
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT je.value)
		FROM memory_retrieval_audit a, json_each(a.chunk_ids) je
		WHERE a.project_id = ? AND a.retrieved_at >= ?`,
		projectID, sqliteTime(since),
	).Scan(&out.RetrievedChunks); err != nil {
		return nil, err
	}
	out.UnretrievedChunks = out.TotalChunks - out.RetrievedChunks
	if out.UnretrievedChunks < 0 {
		out.UnretrievedChunks = 0
	}
	return out, nil
}

// UnretrievedChunkIDs returns chunk IDs indexed for the project
// that haven't appeared in any retrieval audit row since `since`.
// Postgres uses `id NOT IN (SELECT unnest(chunk_ids) ...)`; SQLite
// substitutes json_each for unnest. LIMIT bounds the result so a
// project with thousands of cold chunks doesn't flood the CLI.
func (r *MemoryRetrievalAuditRepository) UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("UnretrievedChunkIDs: projectID is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM project_memory_chunks
		WHERE project_id = ?
		  AND id NOT IN (
		    SELECT DISTINCT je.value
		    FROM memory_retrieval_audit a, json_each(a.chunk_ids) je
		    WHERE a.project_id = ? AND a.retrieved_at >= ?
		  )
		ORDER BY created_at ASC
		LIMIT ?`,
		projectID, projectID, sqliteTime(since), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// List returns retrieval-audit rows matching the filter, newest first.
// SQLite mirror of the postgres impl. PageSize required.
func (r *MemoryRetrievalAuditRepository) List(ctx context.Context, filter persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("MemoryRetrievalAuditRepository.List: PageSize is required")
	}
	args := []any{}
	where := []string{}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, clause)
	}
	if filter.ProjectID != "" {
		add("project_id = ?", filter.ProjectID)
	}
	if filter.ActorKind != "" {
		add("actor_kind = ?", filter.ActorKind)
	}
	if filter.RepoScope != "" {
		add("repo_scope = ?", filter.RepoScope)
	}
	if !filter.Since.IsZero() {
		add("retrieved_at >= ?", sqliteTime(filter.Since))
	}
	q := `
		SELECT id, project_id, task_id, execution_id, step_id, role,
		       query, chunk_ids, retrieved_at,
		       actor_kind, actor_id, repo_scope
		FROM memory_retrieval_audit`
	if len(where) > 0 {
		q += " WHERE " + joinAndSqlite(where)
	}
	q += " ORDER BY retrieved_at DESC LIMIT ? OFFSET ?"
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryRetrievalAudit
	for rows.Next() {
		var a persistence.MemoryRetrievalAudit
		var chunkIDs sqliteStringArray
		var retrievedAt sqlTime
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.TaskID, &a.ExecutionID, &a.StepID, &a.Role,
			&a.Query, &chunkIDs, &retrievedAt,
			&a.ActorKind, &a.ActorID, &a.RepoScope,
		); err != nil {
			return nil, err
		}
		a.RetrievedAt = retrievedAt.Time
		a.ChunkIDs = []string(chunkIDs)
		out = append(out, &a)
	}
	return out, rows.Err()
}
