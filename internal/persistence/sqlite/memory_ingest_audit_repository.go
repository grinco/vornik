package sqlite

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// MemoryIngestAuditRepository persists per-call ingest records for
// the companion-direct deposit path. Companion-direct deposits via
// Indexer.IngestCompanionNote bypass project_ingest_queue, so the
// existing queue + quarantine tables don't capture them; this repo
// closes that audit gap. SQLite mirror of the postgres impl.
type MemoryIngestAuditRepository struct {
	db DBTX
}

func NewMemoryIngestAuditRepository(db DBTX) *MemoryIngestAuditRepository {
	return &MemoryIngestAuditRepository{db: db}
}

// Record inserts one ingest-attempt row.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.ID, audit.ProjectID, audit.ActorKind, audit.ActorID,
		audit.SourceName, audit.ContentHash, audit.ContentBytes,
		audit.ProposedClass, audit.Decision, audit.GateFailed,
		audit.ChunksAdmitted, sqliteTime(audit.IngestedAt), audit.RepoScope,
	)
	return err
}

// ListByProject returns recent ingest-audit rows for one project,
// newest first, capped at limit.
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
		WHERE project_id = ?
		ORDER BY ingested_at DESC
		LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryIngestAudit
	for rows.Next() {
		var a persistence.MemoryIngestAudit
		var ingestedAt sqlTime
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.ActorKind, &a.ActorID,
			&a.SourceName, &a.ContentHash, &a.ContentBytes,
			&a.ProposedClass, &a.Decision, &a.GateFailed,
			&a.ChunksAdmitted, &ingestedAt, &a.RepoScope,
		); err != nil {
			return nil, err
		}
		a.IngestedAt = ingestedAt.Time
		out = append(out, &a)
	}
	return out, rows.Err()
}

// List returns ingest-audit rows matching the filter, newest first.
// SQLite uses ? placeholders so we count manually.
func (r *MemoryIngestAuditRepository) List(ctx context.Context, filter persistence.MemoryIngestAuditFilter) ([]*persistence.MemoryIngestAudit, error) {
	if filter.PageSize <= 0 {
		return nil, fmt.Errorf("MemoryIngestAuditRepository.List: PageSize is required")
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
	if filter.Decision != "" {
		add("decision = ?", filter.Decision)
	}
	if !filter.Since.IsZero() {
		add("ingested_at >= ?", sqliteTime(filter.Since))
	}
	q := `
		SELECT id, project_id, actor_kind, actor_id,
		       source_name, content_hash, content_bytes,
		       proposed_class, decision, gate_failed,
		       chunks_admitted, ingested_at, repo_scope
		FROM memory_ingest_audit`
	if len(where) > 0 {
		q += " WHERE " + joinAndSqlite(where)
	}
	q += " ORDER BY ingested_at DESC LIMIT ? OFFSET ?"
	args = append(args, filter.PageSize, filter.Offset)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*persistence.MemoryIngestAudit
	for rows.Next() {
		var a persistence.MemoryIngestAudit
		var ingestedAt sqlTime
		if err := rows.Scan(
			&a.ID, &a.ProjectID, &a.ActorKind, &a.ActorID,
			&a.SourceName, &a.ContentHash, &a.ContentBytes,
			&a.ProposedClass, &a.Decision, &a.GateFailed,
			&a.ChunksAdmitted, &ingestedAt, &a.RepoScope,
		); err != nil {
			return nil, err
		}
		a.IngestedAt = ingestedAt.Time
		out = append(out, &a)
	}
	return out, rows.Err()
}

// joinAndSqlite concatenates clauses with " AND " — sqlite-package-
// local to avoid colliding with the postgres helper of the same
// intent in the sibling package.
func joinAndSqlite(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " AND " + p
	}
	return out
}
