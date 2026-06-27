package postgres

// Postgres-backed sink for the Policy-Aware Memory Firewall's
// audit rows. The internal/memoryfirewall.AuditWriter calls
// BatchInsert from its flusher goroutine.
//
// The schema lives in migration 80
// (`memory_policy_evaluations`). One row per (chunk, request)
// pair; the writer batches at 50 rows / 100ms. See
// https://docs.vornik.io
// § "New memory_policy_evaluations table".

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// MemoryPolicyEvaluationRepository implements the firewall's
// AuditSink interface against Postgres. Single-statement
// multi-row INSERT per batch to keep the writer goroutine's
// throughput high.
type MemoryPolicyEvaluationRepository struct {
	db persistence.DBTX
}

// NewMemoryPolicyEvaluationRepository wires the repository.
func NewMemoryPolicyEvaluationRepository(db persistence.DBTX) *MemoryPolicyEvaluationRepository {
	return &MemoryPolicyEvaluationRepository{db: db}
}

// BatchInsert writes the supplied rows in a single VALUES list.
// Empty input is a no-op. A driver error short-circuits — the
// writer's caller logs + the buffered rows are dropped (the
// design accepts a small lossy ceiling under DB outage rather
// than unbounded memory growth).
func (r *MemoryPolicyEvaluationRepository) BatchInsert(ctx context.Context, rows []memoryfirewall.EvaluationRow) error {
	if r == nil || r.db == nil {
		return errors.New("memory_policy_evaluations: not configured")
	}
	if len(rows) == 0 {
		return nil
	}

	placeholders := make([]string, len(rows))
	args := make([]any, 0, len(rows)*12)
	for i, row := range rows {
		base := i * 12
		placeholders[i] = fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11, base+12,
		)
		args = append(args,
			row.ID,
			row.ProjectID,
			nullableStringIfEmpty(row.TenantID),
			row.ChunkID,
			nullableStringIfEmpty(row.RequestRole),
			nullableStringIfEmpty(row.RequestPurpose),
			nullableStringIfEmpty(row.RequestOperator),
			nullableStringIfEmpty(row.TraceID),
			string(row.Decision),
			nullableStringIfEmpty(row.PolicyDigest),
			nullableStringIfEmpty(row.ReasonDetail),
			row.EvaluatedAt.UTC(),
		)
	}
	q := fmt.Sprintf(`
INSERT INTO memory_policy_evaluations
    (id, project_id, tenant_id, chunk_id, request_role, request_purpose,
     request_operator, trace_id, decision, policy_digest, reason_detail, evaluated_at)
VALUES %s`, strings.Join(placeholders, ","))

	_, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("memory_policy_evaluations: batch insert: %w", err)
	}
	return nil
}

// ListRecent returns recently-evaluated rows for a project,
// newest first. Supports the operator UI's "recent blocks"
// panel. decisionFilter is optional — empty returns all
// classes; non-empty matches the decision column exactly.
func (r *MemoryPolicyEvaluationRepository) ListRecent(
	ctx context.Context, projectID, decisionFilter string, since time.Time, limit int,
) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args := []any{projectID, since.UTC(), limit}
	q := `
SELECT id, project_id, COALESCE(tenant_id, ''), chunk_id,
       COALESCE(request_role, ''), COALESCE(request_purpose, ''),
       COALESCE(request_operator, ''), COALESCE(trace_id, ''),
       decision, COALESCE(policy_digest, ''), COALESCE(reason_detail, ''),
       evaluated_at
FROM memory_policy_evaluations
WHERE project_id = $1 AND evaluated_at >= $2`
	if decisionFilter != "" {
		q += " AND decision = $4"
		args = append(args, decisionFilter)
	}
	q += " ORDER BY evaluated_at DESC LIMIT $3"

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory_policy_evaluations: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []memoryfirewall.EvaluationRow
	for rows.Next() {
		var row memoryfirewall.EvaluationRow
		var decision string
		if err := rows.Scan(
			&row.ID, &row.ProjectID, &row.TenantID, &row.ChunkID,
			&row.RequestRole, &row.RequestPurpose, &row.RequestOperator, &row.TraceID,
			&decision, &row.PolicyDigest, &row.ReasonDetail, &row.EvaluatedAt,
		); err != nil {
			return nil, fmt.Errorf("memory_policy_evaluations: scan: %w", err)
		}
		row.Decision = memoryfirewall.EvaluationDecision(decision)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListByDigest returns every evaluation row recorded under a policy
// digest, newest first. Low-frequency admin/proof-verifier query
// (firewall LLD § REST endpoints / drift-mitigation §8.3) so the
// digest scan is acceptable; limit clamps the result set.
func (r *MemoryPolicyEvaluationRepository) ListByDigest(
	ctx context.Context, policyDigest string, limit int,
) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `
SELECT id, project_id, COALESCE(tenant_id, ''), chunk_id,
       COALESCE(request_role, ''), COALESCE(request_purpose, ''),
       COALESCE(request_operator, ''), COALESCE(trace_id, ''),
       decision, COALESCE(policy_digest, ''), COALESCE(reason_detail, ''),
       evaluated_at
FROM memory_policy_evaluations
WHERE policy_digest = $1
ORDER BY evaluated_at DESC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, policyDigest, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_policy_evaluations: list by digest: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []memoryfirewall.EvaluationRow
	for rows.Next() {
		var row memoryfirewall.EvaluationRow
		var decision string
		if err := rows.Scan(
			&row.ID, &row.ProjectID, &row.TenantID, &row.ChunkID,
			&row.RequestRole, &row.RequestPurpose, &row.RequestOperator, &row.TraceID,
			&decision, &row.PolicyDigest, &row.ReasonDetail, &row.EvaluatedAt,
		); err != nil {
			return nil, fmt.Errorf("memory_policy_evaluations: scan: %w", err)
		}
		row.Decision = memoryfirewall.EvaluationDecision(decision)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListByChunk returns evaluation rows for one chunk, newest first — backs the
// admin chunk-detail "recent evaluations" panel. Index-backed
// (idx_policy_eval_chunk, migration 102) instead of the prior cross-project
// ListRecent scan + in-process filter.
func (r *MemoryPolicyEvaluationRepository) ListByChunk(
	ctx context.Context, chunkID string, limit int,
) ([]memoryfirewall.EvaluationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("memory_policy_evaluations: not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `
SELECT id, project_id, COALESCE(tenant_id, ''), chunk_id,
       COALESCE(request_role, ''), COALESCE(request_purpose, ''),
       COALESCE(request_operator, ''), COALESCE(trace_id, ''),
       decision, COALESCE(policy_digest, ''), COALESCE(reason_detail, ''),
       evaluated_at
FROM memory_policy_evaluations
WHERE chunk_id = $1
ORDER BY evaluated_at DESC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, chunkID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory_policy_evaluations: list by chunk: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []memoryfirewall.EvaluationRow
	for rows.Next() {
		var row memoryfirewall.EvaluationRow
		var decision string
		if err := rows.Scan(
			&row.ID, &row.ProjectID, &row.TenantID, &row.ChunkID,
			&row.RequestRole, &row.RequestPurpose, &row.RequestOperator, &row.TraceID,
			&decision, &row.PolicyDigest, &row.ReasonDetail, &row.EvaluatedAt,
		); err != nil {
			return nil, fmt.Errorf("memory_policy_evaluations: scan: %w", err)
		}
		row.Decision = memoryfirewall.EvaluationDecision(decision)
		out = append(out, row)
	}
	return out, rows.Err()
}

func nullableStringIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
