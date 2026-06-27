package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// MemoryRetrievalAuditRepository persists per-Search records of
// which memory chunks were returned. Backed by memory_retrieval_audit
// (see deployments/postgres/schema/001_initial.sql).
type MemoryRetrievalAuditRepository struct {
	db DBTX
}

// NewMemoryRetrievalAuditRepository constructs the repo. Accepts the
// shared DBTX abstraction so the same constructor works against the
// instrumented and uninstrumented DB wrappers.
func NewMemoryRetrievalAuditRepository(db DBTX) *MemoryRetrievalAuditRepository {
	return &MemoryRetrievalAuditRepository{db: db}
}

// Record inserts one retrieval row. ID generated when empty so the
// caller can pass partial structs without thinking about ID
// allocation. RetrievedAt defaults to NOW() on the DB side when not
// set, but we set it client-side so test fixtures can override.
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
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		audit.ID, audit.ProjectID, audit.TaskID, audit.ExecutionID, audit.StepID, audit.Role,
		audit.Query, pq.Array(chunks), audit.RetrievedAt,
		audit.ActorKind, audit.ActorID, audit.RepoScope,
	)
	return mapDBError(err)
}

// FeedbackStats joins memory_retrieval_audit with project_memory_chunks
// to produce the chunk-utility summary. The query unnests chunk_ids
// once so we can count distinct retrieved chunks; the rest is
// straight COUNT()s. Projects with no chunks indexed return zeroed
// stats rather than an error so the CLI's empty-state copy reads
// naturally.
func (r *MemoryRetrievalAuditRepository) FeedbackStats(ctx context.Context, projectID string, since time.Time) (*persistence.MemoryFeedbackStats, error) {
	if projectID == "" {
		return nil, fmt.Errorf("FeedbackStats: projectID is required")
	}
	out := &persistence.MemoryFeedbackStats{}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM project_memory_chunks
		WHERE project_id = $1
	`, projectID).Scan(&out.TotalChunks); err != nil {
		return nil, mapDBError(err)
	}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM memory_retrieval_audit
		WHERE project_id = $1 AND retrieved_at >= $2
	`, projectID, since).Scan(&out.TotalSearches); err != nil {
		return nil, mapDBError(err)
	}

	// Distinct chunk IDs returned in the window — unnest the array
	// column and count distinct.
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT chunk_id)
		FROM (
			SELECT unnest(chunk_ids) AS chunk_id
			FROM memory_retrieval_audit
			WHERE project_id = $1 AND retrieved_at >= $2
		) AS retrieved
	`, projectID, since).Scan(&out.RetrievedChunks); err != nil {
		return nil, mapDBError(err)
	}

	out.UnretrievedChunks = out.TotalChunks - out.RetrievedChunks
	if out.UnretrievedChunks < 0 {
		// Defensive: a chunk could appear in retrievals after being
		// pruned from project_memory_chunks. Treat negative as zero
		// so the CLI's "candidates to prune" count never goes
		// nonsense.
		out.UnretrievedChunks = 0
	}
	return out, nil
}

// UnretrievedChunkIDs returns chunk IDs that haven't appeared in any
// retrieval row since `since`. EXCEPT lets us subtract the
// retrieved set from the indexed set in one round-trip; LIMIT bounds
// the result so a project with thousands of cold chunks doesn't
// dump the whole list into the CLI's terminal.
func (r *MemoryRetrievalAuditRepository) UnretrievedChunkIDs(ctx context.Context, projectID string, since time.Time, limit int) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("UnretrievedChunkIDs: projectID is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id
		FROM project_memory_chunks
		WHERE project_id = $1
		  AND id NOT IN (
		    SELECT DISTINCT unnest(chunk_ids)
		    FROM memory_retrieval_audit
		    WHERE project_id = $1 AND retrieved_at >= $2
		  )
		ORDER BY created_at ASC
		LIMIT $3
	`, projectID, since, limit)
	if err != nil {
		return nil, mapDBError(err)
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

// List returns retrieval-audit rows matching the filter, newest
// first. B-16 surfaces this on /ui/admin/memory-audit. Each filter
// axis is optional; PageSize is required and bounds the query.
func (r *MemoryRetrievalAuditRepository) List(ctx context.Context, filter persistence.MemoryRetrievalAuditFilter) ([]*persistence.MemoryRetrievalAudit, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("MemoryRetrievalAuditRepository.List: PageSize is required")
	}
	args := []any{}
	where := []string{}
	addClause := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.ProjectID != "" {
		addClause("project_id = $%d", filter.ProjectID)
	}
	if filter.ActorKind != "" {
		addClause("actor_kind = $%d", filter.ActorKind)
	}
	if filter.RepoScope != "" {
		addClause("repo_scope = $%d", filter.RepoScope)
	}
	if !filter.Since.IsZero() {
		addClause("retrieved_at >= $%d", filter.Since)
	}
	q := `
		SELECT id, project_id, task_id, execution_id, step_id, role,
		       query, chunk_ids, retrieved_at,
		       actor_kind, actor_id, repo_scope
		FROM memory_retrieval_audit
	`
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += fmt.Sprintf(" ORDER BY retrieved_at DESC LIMIT $%d OFFSET $%d",
		len(args)+1, len(args)+2)
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryRetrievalAudit
	for rows.Next() {
		var a persistence.MemoryRetrievalAudit
		var chunkIDs pq.StringArray
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.TaskID, &a.ExecutionID, &a.StepID, &a.Role,
			&a.Query, &chunkIDs, &a.RetrievedAt,
			&a.ActorKind, &a.ActorID, &a.RepoScope,
		); err != nil {
			return nil, mapDBError(err)
		}
		a.ChunkIDs = []string(chunkIDs)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// joinAnd concatenates clauses with " AND " — kept here so the
// formatter+linter sees a single tight function instead of inline
// string ops in List.
func joinAnd(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " AND " + p
	}
	return out
}
