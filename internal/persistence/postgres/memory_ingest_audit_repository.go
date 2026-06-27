package postgres

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemoryIngestAuditRepository persists per-call ingest records for
// the companion-direct deposit path. Backed by memory_ingest_audit
// (migration 74). Companion-direct deposits via
// Indexer.IngestCompanionNote bypass project_ingest_queue, so the
// existing queue + quarantine tables don't capture them; this repo
// closes that audit gap.
type MemoryIngestAuditRepository struct {
	db DBTX
}

// NewMemoryIngestAuditRepository constructs the repo. Accepts the
// shared DBTX abstraction so the same constructor works against the
// instrumented and uninstrumented DB wrappers.
func NewMemoryIngestAuditRepository(db DBTX) *MemoryIngestAuditRepository {
	return &MemoryIngestAuditRepository{db: db}
}

// Record inserts one ingest-attempt row. ID generated when empty so
// the caller can pass partial structs without thinking about ID
// allocation. IngestedAt defaults to NOW() on the DB side when not
// set, but we set it client-side so test fixtures can override.
func (r *MemoryIngestAuditRepository) Record(ctx context.Context, audit *persistence.MemoryIngestAudit) error {
	if audit == nil {
		return fmt.Errorf("MemoryIngestAuditRepository.Record: audit is nil")
	}
	if audit.ID == "" {
		audit.ID = persistence.GenerateID("ming")
	}
	if audit.IngestedAt.IsZero() {
		audit.IngestedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO memory_ingest_audit (
			id, project_id, actor_kind, actor_id,
			source_name, content_hash, content_bytes,
			proposed_class, decision, gate_failed,
			chunks_admitted, ingested_at, repo_scope
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`,
		audit.ID, audit.ProjectID, audit.ActorKind, audit.ActorID,
		audit.SourceName, audit.ContentHash, audit.ContentBytes,
		audit.ProposedClass, audit.Decision, audit.GateFailed,
		audit.ChunksAdmitted, audit.IngestedAt, audit.RepoScope,
	)
	return mapDBError(err)
}

// ListByProject returns recent ingest-audit rows for one project,
// newest first, capped at limit. Powers the /ui/memory audit
// dashboard's ingest-side panel.
func (r *MemoryIngestAuditRepository) ListByProject(ctx context.Context, projectID string, limit int) ([]*persistence.MemoryIngestAudit, error) {
	if projectID == "" {
		return nil, fmt.Errorf("ListByProject: projectID is required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, project_id, actor_kind, actor_id,
		       source_name, content_hash, content_bytes,
		       proposed_class, decision, gate_failed,
		       chunks_admitted, ingested_at, repo_scope
		FROM memory_ingest_audit
		WHERE project_id = $1
		ORDER BY ingested_at DESC
		LIMIT $2
	`, projectID, limit)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryIngestAudit
	for rows.Next() {
		var a persistence.MemoryIngestAudit
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.ActorKind, &a.ActorID,
			&a.SourceName, &a.ContentHash, &a.ContentBytes,
			&a.ProposedClass, &a.Decision, &a.GateFailed,
			&a.ChunksAdmitted, &a.IngestedAt, &a.RepoScope,
		); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// List returns ingest-audit rows matching the filter, newest first.
// B-16's /ui/admin/memory-audit ingest panel. PageSize required.
func (r *MemoryIngestAuditRepository) List(ctx context.Context, filter persistence.MemoryIngestAuditFilter) ([]*persistence.MemoryIngestAudit, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("MemoryIngestAuditRepository.List: PageSize is required")
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
	if filter.Decision != "" {
		addClause("decision = $%d", filter.Decision)
	}
	if !filter.Since.IsZero() {
		addClause("ingested_at >= $%d", filter.Since)
	}
	q := `
		SELECT id, project_id, actor_kind, actor_id,
		       source_name, content_hash, content_bytes,
		       proposed_class, decision, gate_failed,
		       chunks_admitted, ingested_at, repo_scope
		FROM memory_ingest_audit
	`
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += fmt.Sprintf(" ORDER BY ingested_at DESC LIMIT $%d OFFSET $%d",
		len(args)+1, len(args)+2)
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryIngestAudit
	for rows.Next() {
		var a persistence.MemoryIngestAudit
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.ActorKind, &a.ActorID,
			&a.SourceName, &a.ContentHash, &a.ContentBytes,
			&a.ProposedClass, &a.Decision, &a.GateFailed,
			&a.ChunksAdmitted, &a.IngestedAt, &a.RepoScope,
		); err != nil {
			return nil, mapDBError(err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}
