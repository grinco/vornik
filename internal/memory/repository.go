package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Repository handles all database operations for the memory system.
// It uses database/sql with lib/pq and speaks directly to pgvector via
// text-literal vector syntax rather than importing a pgvector-go driver.
type Repository struct {
	db            *sql.DB
	pgvectorOnce  sync.Once
	pgvectorAvail bool
}

// NewRepository creates a new Repository.
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// pgvectorAvailable detects whether the pgvector extension is installed.
// The result is cached after the first call.
func (r *Repository) pgvectorAvailable(ctx context.Context) bool {
	r.pgvectorOnce.Do(func() {
		var exists bool
		err := r.db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='vector')").Scan(&exists)
		if err == nil {
			r.pgvectorAvail = exists
		}
	})
	return r.pgvectorAvail
}

// UpsertChunks inserts chunks into project_memory_chunks, skipping duplicates
// identified by (project_id, content_hash).
func (r *Repository) UpsertChunks(ctx context.Context, chunks []MemoryChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	const q = `
INSERT INTO project_memory_chunks
    (id, project_id, task_id, artifact_id, source_name, chunk_index, content, content_hash,
     needs_graph_extraction,
     derived_from_extracted_document_id, derived_from_section_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, $9, $10)
ON CONFLICT (project_id, content_hash) DO NOTHING`

	for _, c := range chunks {
		// Optional FK columns must arrive as NULL when empty.
		// Legacy callers (the markdown-OUTPUT path via
		// IngestText/IngestFile) populate TaskID and ArtifactID
		// from task.ID and artifact.ID — always non-empty.
		// IngestExtractedSections triggered via the operator API
		// has no task context, so passing "" would violate
		// project_memory_chunks_task_id_fkey. Same for the
		// derived_from_* provenance pointers introduced alongside
		// the document-extraction pipeline.
		_, err := r.db.ExecContext(ctx, q,
			c.ID, c.ProjectID,
			nullableString(c.TaskID), nullableString(c.ArtifactID),
			c.SourceName, c.ChunkIndex, c.Content, c.ContentHash,
			nullableString(c.DerivedFromExtractedDocumentID),
			nullableString(c.DerivedFromSectionID),
		)
		if err != nil {
			return fmt.Errorf("upsert chunk %s: %w", c.ID, err)
		}
	}
	return nil
}

// nullableString turns "" into a typed nil so the SQL driver emits
// NULL for FK / nullable columns. A bare "" goes through as the
// empty-string literal, which fails FK checks against task / artifact
// rows that no longer (or never did) carry such an id.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoadChunkPolicies fetches the policy columns added by
// migration 79 for the supplied chunk IDs. Returns a map keyed
// on chunk_id. Missing chunks are absent from the map; the
// caller treats them as "default policy" via the firewall's
// DefaultPolicyForSource (lazy backfill — pre-migration-79
// chunks need no explicit operator action).
//
// Phase A of the Policy-Aware Memory Firewall (LLD:
// https://docs.vornik.io).
// Read-only; the policy columns are nullable for backwards
// compatibility so NULL is the expected value for legacy chunks.
func (r *Repository) LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]ChunkPolicyRow, error) {
	if len(chunkIDs) == 0 {
		return map[string]ChunkPolicyRow{}, nil
	}
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, len(chunkIDs))
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	q := fmt.Sprintf(`
SELECT id,
       COALESCE(tenant_id, ''),
       COALESCE(sensitivity_tier, ''),
       COALESCE(provenance_source, ''),
       COALESCE(provenance_producer, ''),
       COALESCE(provenance_trust, 0),
       COALESCE(provenance_url, ''),
       firewall_expires_at,
       permitted_roles,
       allowed_purposes,
       COALESCE(policy_digest, ''),
       COALESCE(content_class, ''),
       COALESCE(validation_status, '')
FROM project_memory_chunks
WHERE id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load chunk policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]ChunkPolicyRow, len(chunkIDs))
	for rows.Next() {
		var row ChunkPolicyRow
		var expiresAt sql.NullTime
		var roles, purposes []sql.NullString
		// pq.Array would be ideal but the repository's existing
		// query helpers don't import pq directly. Scan into a
		// nullable text + parse via a small array decoder.
		var rolesArr, purposesArr sql.NullString
		if err := rows.Scan(
			&row.ChunkID,
			&row.TenantID,
			&row.SensitivityTier,
			&row.ProvenanceSource,
			&row.ProvenanceProducer,
			&row.ProvenanceTrust,
			&row.ProvenanceURL,
			&expiresAt,
			&rolesArr,
			&purposesArr,
			&row.PolicyDigest,
			&row.ContentClass,
			&row.ValidationStatus,
		); err != nil {
			return nil, fmt.Errorf("scan chunk policy: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			row.FirewallExpiresAt = &t
		}
		row.PermittedRoles = parsePqArray(rolesArr.String)
		row.AllowedPurposes = parsePqArray(purposesArr.String)
		_ = roles
		_ = purposes
		out[row.ChunkID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter chunk policies: %w", err)
	}
	return out, nil
}

// ChunkPolicyRow is the row shape LoadChunkPolicies returns.
// Flat structure mirrors the SQL projection 1:1 so adding new
// policy columns is a one-line append here + one new SELECT
// column.
type ChunkPolicyRow struct {
	ChunkID            string
	TenantID           string
	SensitivityTier    string
	ProvenanceSource   string
	ProvenanceProducer string
	ProvenanceTrust    int
	ProvenanceURL      string
	FirewallExpiresAt  *time.Time
	PermittedRoles     []string
	AllowedPurposes    []string
	PolicyDigest       string
	// ContentClass + ValidationStatus are loaded alongside the
	// firewall columns so the caller's ApplyClassifierSignal
	// can override sensitivity to Restricted on credentials
	// chunks without a second query.
	ContentClass     string
	ValidationStatus string
}

// parsePqArray decodes the Postgres array literal form
// `{a,b,c}` into a Go []string. Empty/NULL arrays return nil
// (the firewall's "no restriction" semantic).
//
// Lives here rather than in pgarray.go because the existing
// repository uses sql.NullString instead of pq.Array for the
// fields it cares about; adopting pq.Array everywhere is a
// separate refactor.
func parsePqArray(s string) []string {
	if s == "" || s == "{}" {
		return nil
	}
	// Drop the outer braces.
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	// Naive split on ',' — works because the firewall's
	// permitted_roles + allowed_purposes elements are
	// identifier-shaped (no commas or quoting needed).
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Strip optional double-quotes Postgres uses when an
		// element contains special chars.
		p = strings.Trim(p, `"`)
		out = append(out, p)
	}
	return out
}

// EnqueueForEmbedding inserts chunk IDs into the embed queue.
// Existing entries are ignored (ON CONFLICT DO NOTHING).
func (r *Repository) EnqueueForEmbedding(ctx context.Context, chunkIDs []string) error {
	if len(chunkIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(chunkIDs))
	args := make([]interface{}, len(chunkIDs))
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`
INSERT INTO memory_embed_queue (chunk_id, project_id)
SELECT id, project_id FROM project_memory_chunks WHERE id IN (%s)
ON CONFLICT (chunk_id) DO NOTHING`, strings.Join(placeholders, ","))

	_, err := r.db.ExecContext(ctx, query, args...)
	return err
}

// RequeueAllForEmbedding enqueues every chunk in projectID for
// re-embedding. Used by `vornikctl memory reembed` when an operator
// upgrades the embedding model or dimension and wants existing chunks
// rebuilt with the new model. Returns the number of rows added to
// memory_embed_queue. Existing queue entries are preserved
// (ON CONFLICT DO NOTHING) so a half-finished re-embed doesn't
// duplicate work.
//
// Bounded by an explicit projectID so a typo can't accidentally
// re-embed every chunk in the deployment.
func (r *Repository) RequeueAllForEmbedding(ctx context.Context, projectID string) (int, error) {
	if r == nil || r.db == nil || projectID == "" {
		return 0, nil
	}
	const q = `
INSERT INTO memory_embed_queue (chunk_id, project_id)
SELECT id, project_id FROM project_memory_chunks
WHERE project_id = $1
ON CONFLICT (chunk_id) DO NOTHING`
	res, err := r.db.ExecContext(ctx, q, projectID)
	if err != nil {
		return 0, fmt.Errorf("RequeueAllForEmbedding: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DequeueEmbedBatch atomically selects and deletes up to limit rows from the
// embed queue using SKIP LOCKED to allow safe concurrent workers.
// DequeueEmbedBatch atomically pulls a batch of pending embed
// rows from memory_embed_queue and loads the corresponding chunks
// from project_memory_chunks. Both queries run inside a single
// transaction: if anything between the DELETE-RETURNING and the
// final chunk SELECT fails (process crash, ctx cancellation,
// network blip, scan error), the whole tx rolls back and the
// queue rows are preserved for the next worker tick.
//
// Pre-2026-05-29 (audit-agent finding): the DELETE-RETURNING and
// fetchChunksByIDs were separate auto-committed statements.
// A crash between them would purge the queue rows + the worker
// got nothing back, silently leaving the chunks permanently
// un-embedded with no DLQ trace.
func (r *Repository) DequeueEmbedBatch(ctx context.Context, limit int) ([]MemoryChunk, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("dequeue embed batch: begin tx: %w", err)
	}
	// Defensive rollback — harmless no-op if the explicit Commit
	// below already fired. Without this, a panic in the scan loop
	// would leak the transaction.
	defer func() { _ = tx.Rollback() }()

	const deleteQ = `
DELETE FROM memory_embed_queue
WHERE chunk_id IN (
    SELECT chunk_id FROM memory_embed_queue
    ORDER BY enqueued_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING chunk_id`

	rows, err := tx.QueryContext(ctx, deleteQ, limit)
	if err != nil {
		return nil, fmt.Errorf("dequeue embed batch: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rerr := rows.Err()
	_ = rows.Close()
	if rerr != nil {
		return nil, rerr
	}
	if len(ids) == 0 {
		// Nothing to do — committing the (empty) tx is fine; the
		// SELECT-with-FOR-UPDATE held a brief row lock that the
		// commit releases.
		return nil, tx.Commit()
	}

	chunks, err := fetchChunksByIDsTx(ctx, tx, ids)
	if err != nil {
		// Rolls back via the deferred Rollback above — queue rows
		// stay intact for the next tick to re-claim.
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("dequeue embed batch: commit: %w", err)
	}
	return chunks, nil
}

// fetchChunksByIDsTx is the tx-bound chunk loader used inside
// DequeueEmbedBatch's transaction. Originally there was also a
// standalone *sql.DB variant (fetchChunksByIDs) but the dequeue
// tx is the only caller — keep just the tx form. The query runs
// inside the same tx as the DELETE-RETURNING so an error aborts
// the whole dequeue.
func fetchChunksByIDsTx(ctx context.Context, tx *sql.Tx, ids []string) ([]MemoryChunk, error) {
	return fetchChunksByIDsExec(ctx, tx, ids)
}

// chunkQueryRunner abstracts *sql.DB and *sql.Tx so the same fetch
// logic serves both auto-committed and transactional callers.
// Stays unexported — caller-facing API is the wrapper above.
type chunkQueryRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func fetchChunksByIDsExec(ctx context.Context, q chunkQueryRunner, ids []string) ([]MemoryChunk, error) {
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := fmt.Sprintf(`
SELECT id, project_id, COALESCE(task_id,''), COALESCE(artifact_id,''), source_name,
       chunk_index, content, content_hash, created_at
FROM project_memory_chunks
WHERE id IN (%s)`, strings.Join(placeholders, ","))

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch chunks by ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chunks []MemoryChunk
	for rows.Next() {
		var c MemoryChunk
		if err := rows.Scan(
			&c.ID, &c.ProjectID, &c.TaskID, &c.ArtifactID,
			&c.SourceName, &c.ChunkIndex, &c.Content, &c.ContentHash,
			&c.CreatedAt,
		); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// UpdateEmbedding stores the embedding vector for a chunk using pgvector's
// text literal format '[0.1, 0.2, ...]'::vector.
// ErrEmptyEmbedding is returned by UpdateEmbedding when called with
// a zero-length slice. Used by the worker to distinguish "model
// returned empty vector for this chunk" (a real signal worth
// logging + DLQ-ing) from "embedding stored OK". 2026-05-29 audit
// fix: pre-fix UpdateEmbedding silently returned nil on empty
// input, making un-embedded chunks indistinguishable from
// successfully-embedded ones at the caller boundary.
var ErrEmptyEmbedding = fmt.Errorf("memory repo: refusing to store empty embedding")

func (r *Repository) UpdateEmbedding(ctx context.Context, chunkID string, embedding []float32) error {
	if len(embedding) == 0 {
		return ErrEmptyEmbedding
	}
	lit := vectorLiteral(embedding)
	// P1: Parameterize the vector literal instead of fmt.Sprintf to eliminate
	// SQL injection risk.
	query := `UPDATE project_memory_chunks SET embedding = $1::vector WHERE id = $2`
	_, err := r.db.ExecContext(ctx, query, lit, chunkID)
	return err
}

// StampDefaultPoliciesForNewChunks applies a default policy to
// chunks that don't yet carry an explicit one. Used by the
// Indexer's post-insert hook so newly-ingested chunks land
// with full policy metadata instead of relying on the read-
// time lazy-backfill via DefaultPolicyForSource.
//
// The WHERE policy_digest IS NULL clause is the operator-edit
// guard: once an admin has set policy via UpdateChunkPolicy
// (which stamps a digest), this method won't touch it. Lets
// operator edits stick across re-ingest of the same content.
//
// Phase 2026.5.9 follow-on of the Policy-Aware Memory Firewall.
// Returns rows-affected so callers can confirm the stamp landed
// (0 = every chunk in the batch already had a digest, which is
// the operator-edit-stuck case).
func (r *Repository) StampDefaultPoliciesForNewChunks(
	ctx context.Context,
	chunkIDs []string,
	tenantID, sensitivityTier, provenanceSource, provenanceProducer string,
	provenanceTrust int,
	provenanceURL string,
	firewallExpiresAt *time.Time,
	permittedRoles, allowedPurposes []string,
	policyDigest string,
) (int64, error) {
	if r == nil || r.db == nil || len(chunkIDs) == 0 || policyDigest == "" {
		return 0, nil
	}
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+10)
	args = append(args,
		nullableString(tenantID),
		nullableString(sensitivityTier),
		nullableString(provenanceSource),
		nullableString(provenanceProducer),
		provenanceTrust,
		nullableString(provenanceURL),
		nullableTime(timeOrNil(firewallExpiresAt)),
		pqStringArray(permittedRoles),
		pqStringArray(allowedPurposes),
		policyDigest,
	)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+11)
		args = append(args, id)
	}
	q := fmt.Sprintf(`
UPDATE project_memory_chunks
   SET tenant_id            = $1,
       sensitivity_tier     = $2,
       provenance_source    = $3,
       provenance_producer  = $4,
       provenance_trust     = $5,
       provenance_url       = $6,
       firewall_expires_at  = $7,
       permitted_roles      = $8,
       allowed_purposes     = $9,
       policy_digest        = $10
 WHERE id IN (%s)
   AND policy_digest IS NULL`, strings.Join(placeholders, ","))
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("stamp default policies: %w", err)
	}
	return res.RowsAffected()
}

// UpdateChunkPolicy mutates the firewall policy columns on one
// chunk. Recomputes policy_digest from the supplied row; the
// caller is responsible for the canonicalisation (calls
// memoryfirewall.PolicyDigest before invoking this method).
//
// Phase D follow-on of the Policy-Aware Memory Firewall (LLD §
// "API — POST /api/v1/admin/memory/policy/chunks/{id}"). Wires
// the persistence side of per-chunk policy editing; the API
// handler + audit-row emission live in the api package.
//
// Empty digest is invalid — pass the result of
// memoryfirewall.PolicyDigest(row) to keep the proof chain
// stable. Nil time pointer = "no expiry" (column set NULL).
// Nil slices = "no restriction" on permitted_roles /
// allowed_purposes (also NULL).
//
// Returns the row count affected (0 = chunk not found; >0 = OK)
// so the API handler can return 404 distinctly from a DB error.
func (r *Repository) UpdateChunkPolicy(ctx context.Context, row ChunkPolicyRow) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("memory repo: not configured")
	}
	if row.ChunkID == "" {
		return 0, fmt.Errorf("memory repo: UpdateChunkPolicy: chunk_id required")
	}
	if row.PolicyDigest == "" {
		return 0, fmt.Errorf("memory repo: UpdateChunkPolicy: policy_digest required (caller computes via memoryfirewall.PolicyDigest)")
	}
	const q = `
UPDATE project_memory_chunks
   SET tenant_id            = $2,
       sensitivity_tier     = $3,
       provenance_source    = $4,
       provenance_producer  = $5,
       provenance_trust     = $6,
       provenance_url       = $7,
       firewall_expires_at  = $8,
       permitted_roles      = $9,
       allowed_purposes     = $10,
       policy_digest        = $11
 WHERE id = $1`
	res, err := r.db.ExecContext(ctx, q,
		row.ChunkID,
		nullableString(row.TenantID),
		nullableString(row.SensitivityTier),
		nullableString(row.ProvenanceSource),
		nullableString(row.ProvenanceProducer),
		row.ProvenanceTrust,
		nullableString(row.ProvenanceURL),
		nullableTime(timeOrNil(row.FirewallExpiresAt)),
		pqStringArray(row.PermittedRoles),
		pqStringArray(row.AllowedPurposes),
		row.PolicyDigest,
	)
	if err != nil {
		return 0, fmt.Errorf("update chunk policy: %w", err)
	}
	return res.RowsAffected()
}

// timeOrNil returns t when non-nil + non-zero, else the zero
// time.Time (which nullableTime translates to SQL NULL).
func timeOrNil(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// UpdateContentTitle stores the LLM-generated topic label for a chunk.
// Empty title is a no-op so callers can blindly pass the Titler's
// output (which is "" on fall-through failure).
func (r *Repository) UpdateContentTitle(ctx context.Context, chunkID, title string) error {
	if r == nil || r.db == nil || chunkID == "" {
		return nil
	}
	t := strings.TrimSpace(title)
	if t == "" {
		return nil
	}
	const q = `UPDATE project_memory_chunks SET content_title = $1 WHERE id = $2`
	_, err := r.db.ExecContext(ctx, q, t, chunkID)
	return err
}

// TitleBackfillRow is a slim row shape used by the backfill CLI:
// just the columns the titler needs (id + content) plus a couple of
// fields useful for human progress output.
type TitleBackfillRow struct {
	ID         string
	ProjectID  string
	SourceName string
	Content    string
}

// CountChunksMissingTitle returns the number of chunks across all
// projects with NULL content_title. Used by the backfill CLI for a
// progress denominator.
func (r *Repository) CountChunksMissingTitle(ctx context.Context) (int, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	const q = `
		SELECT COUNT(*) FROM project_memory_chunks
		WHERE content_title IS NULL`
	var n int
	if err := r.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountChunksMissingTitle: %w", err)
	}
	return n, nil
}

// ListChunksMissingTitle returns up to limit chunks with NULL
// content_title, ordered by created_at ASC so the backfill processes
// the oldest first (predictable, resumable). Limit ≤ 0 is treated as
// 100; values > 1000 are capped to keep memory bounded.
func (r *Repository) ListChunksMissingTitle(ctx context.Context, limit int) ([]TitleBackfillRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `
		SELECT id, project_id, source_name, content
		FROM project_memory_chunks
		WHERE content_title IS NULL
		ORDER BY created_at ASC
		LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("ListChunksMissingTitle: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TitleBackfillRow
	for rows.Next() {
		var row TitleBackfillRow
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.SourceName, &row.Content); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// VizChunk is the per-chunk row the operator-viz endpoints consume.
// Lighter than MemoryChunk: just enough to render a scatter point
// + tooltip, no embedding payload (the caller asks for that
// separately when needed).
type VizChunk struct {
	ID               string
	SourceName       string
	DisplayTitle     string // resolved label: content_title → H1/H2 heading → SourceName
	ContentClass     string
	ValidationStatus string
	ProducerRole     string
	Preview          string
	ContentSize      int       // length of the full content (not the preview cap)
	Embedding        []float32 // populated when withEmbedding=true
}

// extractContentTitle resolves the human-readable display label for a
// chunk. Precedence:
//  1. contentTitle — the LLM-generated topic label persisted at
//     ingest by the Titler. Wins when non-empty.
//  2. First markdown H1/H2 in preview — kept as a free fallback for
//     docs whose authors already provided a clear heading.
//  3. fallback (typically the artifact filename).
func extractContentTitle(contentTitle, preview, fallback string) string {
	if t := strings.TrimSpace(contentTitle); t != "" {
		return t
	}
	for _, line := range strings.SplitN(preview, "\n", 20) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			t := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if t != "" {
				return t
			}
		}
		if strings.HasPrefix(line, "# ") {
			t := strings.TrimSpace(strings.TrimPrefix(line, "# "))
			if t != "" {
				return t
			}
		}
	}
	return fallback
}

// SampleChunksForViz returns up to limit chunks for the viz panels.
// When withEmbedding=true, the embedding column is read too — used
// by the PCA scatter. Random sample (ORDER BY random()) so a
// 100k-chunk corpus doesn't OOM the request, and so successive
// page loads see different cross-sections (the rest of the corpus
// isn't hidden, just not in this sample).
//
// Reads only from active epochs + non-superseded/refuted chunks
// (matching the search filter) so the viz reflects what's actually
// retrievable, not the full table.
func (r *Repository) SampleChunksForViz(ctx context.Context, projectID string, activeEpochs []string, withEmbedding bool, limit int) ([]VizChunk, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}

	cols := "id, source_name, COALESCE(content_title, ''), content_class, COALESCE(validation_status, 'unverified'), COALESCE(producer_role, ''), substring(content, 1, 600), char_length(content)"
	embedFilter := ""
	if withEmbedding {
		cols += ", embedding::text"
		embedFilter = "AND embedding IS NOT NULL"
	}

	q := `
		SELECT ` + cols + `
		FROM project_memory_chunks
		WHERE project_id = $1
		  AND lifecycle_state = 'published'
		  AND validation_status NOT IN ('refuted','superseded')
		  AND (expires_at IS NULL OR expires_at > NOW())
		  AND (epoch_id IS NULL OR epoch_id = ANY($2::text[]))
		  ` + embedFilter + `
		ORDER BY random()
		LIMIT $3`

	rows, err := r.db.QueryContext(ctx, q, projectID, pqStringArray(activeEpochs), limit)
	if err != nil {
		return nil, fmt.Errorf("SampleChunksForViz: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []VizChunk
	for rows.Next() {
		c := VizChunk{}
		var contentTitle string
		var embedding sql.NullString
		if withEmbedding {
			if err := rows.Scan(&c.ID, &c.SourceName, &contentTitle, &c.ContentClass, &c.ValidationStatus, &c.ProducerRole, &c.Preview, &c.ContentSize, &embedding); err != nil {
				return nil, err
			}
			if embedding.Valid {
				c.Embedding = parseVectorLiteral(embedding.String)
			}
		} else {
			if err := rows.Scan(&c.ID, &c.SourceName, &contentTitle, &c.ContentClass, &c.ValidationStatus, &c.ProducerRole, &c.Preview, &c.ContentSize); err != nil {
				return nil, err
			}
		}
		c.DisplayTitle = extractContentTitle(contentTitle, c.Preview, c.SourceName)
		out = append(out, c)
	}
	return out, rows.Err()
}

// parseVectorLiteral parses pgvector's text format "[1.0,2.0,3.0]"
// into []float32. Returns nil on any parse failure — callers treat
// nil as "no embedding for this row" rather than aborting the whole
// viz batch.
func parseVectorLiteral(s string) []float32 {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil
		}
		out = append(out, float32(v))
	}
	return out
}

// MarkVerifiedByArtifact flips validation_status to 'verified' on
// every chunk for the given artifact. Used when the producer role
// is role_of_record (e.g. reviewer/architect for Decision class) —
// their writes land verified directly, bypassing the LLM validator.
// MDM "system of record" pattern. Does NOT downgrade chunks
// already at terminal states (superseded/refuted) — those win.
func (r *Repository) MarkVerifiedByArtifact(ctx context.Context, projectID, artifactID, validatorRole string) error {
	if r == nil || r.db == nil || projectID == "" || artifactID == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET validation_status = 'verified',
		    validator_role    = COALESCE(NULLIF($3, ''), validator_role)
		WHERE project_id = $1 AND artifact_id = $2
		  AND validation_status NOT IN ('superseded','refuted')
	`, projectID, artifactID, validatorRole)
	return err
}

// SupersedeBySameSource marks older chunks superseded when a
// fresh chunk lands for the same (project_id, content_class,
// source_name). The new chunk's artifact ID is excluded so the
// fresh write isn't included in its own supersession set.
//
// Same-source supersession is a deterministic Phase 4 simplification
// of the design's cosine-similarity supersession (§4 #12). Cosine
// supersession ships when re-embedding cadence guarantees the new
// chunk has its embedding before the supersession query runs;
// today, embeddings land asynchronously so we use the source-name
// proxy for reliability.
//
// Disambig-aware matching (2026-05-16): the executor's artifact
// harvest now appends a -YYYYMMDD-XXXX suffix to every workspace
// filename so independent tasks producing the same logical name
// don't collide in retrieval. That makes the literal source_name
// vary across executions for the same logical artifact, breaking
// the legacy exact-match supersede. The match below recognises
// both forms: a chunk whose source_name has the same {stem}{ext}
// as the incoming sourceName (after stripping its disambig suffix)
// is treated as the older version. Legacy chunks indexed before
// disambiguation rolled out match the un-stripped path naturally.
//
// epochID is the epoch the superseding chunks landed in — recorded as
// supersession provenance (migration 89) so RollbackTo can restore the
// prior version when that epoch is rolled back. Empty epochID (an
// epochless ingest) records NULL provenance: that supersession is
// non-restorable, same as pre-migration history, and the rollback
// preview reports it. See
// https://docs.vornik.io
//
// Returns the count of chunks marked superseded.
func (r *Repository) SupersedeBySameSource(ctx context.Context, projectID, contentClass, sourceName, taskID, newArtifactID, epochID string) (int, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	if projectID == "" || sourceName == "" || newArtifactID == "" || contentClass == "" || taskID == "" {
		return 0, nil
	}
	stem, ext := splitSourceNameForSupersede(sourceName)
	// LIKE pattern: {stem}-________-____{ext} matches the
	// 8-digit date + 4-hex-id disambig suffix. The legacy form
	// {stem}{ext} is matched via the OR exact-equal branch so
	// chunks indexed before the 2026-05-16 disambig roll-out
	// still supersede correctly when a re-run lands. Trailing
	// '%' is NOT used — the disambig suffix is fixed-length so
	// the wildcard count gives an exact-shape match without
	// over-matching unrelated names sharing the stem.
	likePattern := stem + "-________-____" + ext
	legacyExact := stem + ext
	// Supersession is scoped to the same task_id so that independent
	// tasks writing the same filename (e.g. two different research.md
	// artifacts) do not wipe each other's chunks. Only a re-run of the
	// same task (same task_id producing a fresh artifact) suppresses its
	// own previous output.
	// Provenance capture (migration 89, 2026-06-04 bug-sweep critical
	// finding): pre_supersede_status reads the PRE-update value
	// (standard SQL — SET expressions see the old row), so restore
	// puts back exactly what was there; superseded_in_epoch keys the
	// rollback restore pass on the causing epoch.
	res, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET validation_status    = 'superseded',
		    pre_supersede_status = validation_status,
		    superseded_in_epoch  = NULLIF($7, '')
		WHERE project_id    = $1
		  AND content_class = $2
		  AND task_id       = $3
		  AND artifact_id  != $4
		  AND (source_name = $5 OR source_name LIKE $6)
		  AND validation_status NOT IN ('superseded','refuted','legacy')
	`, projectID, contentClass, taskID, newArtifactID, legacyExact, likePattern, epochID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// splitSourceNameForSupersede separates an artifact source_name
// into (stem, ext) for the supersession LIKE pattern. If the
// name already carries the -YYYYMMDD-XXXX disambig suffix, the
// suffix is stripped so the match catches both legacy chunks
// (un-suffixed) and prior disambig'd chunks (suffixed but for a
// different execution).
//
// Recognises the disambig shape strictly to avoid false positives
// on operator-named files like `report-stats-1ab2.md` that happen
// to look suffix-shaped: requires exactly 8 digits + 4 lowercase
// hex chars in the slot.
func splitSourceNameForSupersede(name string) (stem, ext string) {
	// Detect ext (last dot, with leading-dot files treated as no-ext).
	dot := -1
	if idx := lastIndexByte(name, '.'); idx > 0 {
		dot = idx
	}
	base := name
	if dot > 0 {
		base = name[:dot]
		ext = name[dot:]
	}
	// Try to strip a trailing `-YYYYMMDD-XXXX` from base.
	const suffixLen = 1 + 8 + 1 + 4 // -DDDDDDDD-XXXX
	if len(base) > suffixLen {
		tail := base[len(base)-suffixLen:]
		if tail[0] == '-' && tail[9] == '-' && allDigits(tail[1:9]) && allLowerHex(tail[10:]) {
			return base[:len(base)-suffixLen], ext
		}
	}
	return base, ext
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func allLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// StampEpochByArtifact sets epoch_id on every chunk for the given
// artifact. Used by the Pipeline after IngestText so chunks carry
// the epoch they were admitted in. Idempotent: re-stamping the
// same epoch is a no-op; stamping over a different epoch (rare)
// reflects the most recent admission.
func (r *Repository) StampEpochByArtifact(ctx context.Context, projectID, artifactID, epochID string) error {
	if r == nil || r.db == nil || projectID == "" || artifactID == "" || epochID == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET epoch_id = $3
		WHERE project_id = $1 AND artifact_id = $2
		  AND (epoch_id IS NULL OR epoch_id <> $3)
	`, projectID, artifactID, epochID)
	return err
}

// ClassifyBackfillRow is the slim row shape used by the LLM
// classifier backfill: chunk id + content + the provenance fields
// that go into the classifier prompt (source_name, producer_role).
type ClassifyBackfillRow struct {
	ID           string
	ProjectID    string
	SourceName   string
	ProducerRole string
	Content      string
}

// ListUnclassifiedChunks returns up to limit chunks whose
// content_class is 'unclassified' or empty for projectID, ordered
// oldest-first so the backfill is resumable. limit ≤ 0 → 100,
// capped at 1000 to bound memory per batch.
func (r *Repository) ListUnclassifiedChunks(ctx context.Context, projectID string, limit int) ([]ClassifyBackfillRow, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `
SELECT id, project_id,
       COALESCE(source_name, ''),
       COALESCE(producer_role, ''),
       content
FROM project_memory_chunks
WHERE project_id = $1
  AND COALESCE(content_class, '') IN ('', 'unclassified')
ORDER BY created_at ASC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("ListUnclassifiedChunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClassifyBackfillRow
	for rows.Next() {
		var row ClassifyBackfillRow
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.SourceName, &row.ProducerRole, &row.Content); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountUnclassifiedChunks returns the count of chunks across all
// projects whose content_class is 'unclassified' or empty. Used by
// the auto-backfill loop to set the prometheus remaining-gauge and
// short-circuit ticks when there's nothing to do (mirrors
// CountChunksMissingTitle).
func (r *Repository) CountUnclassifiedChunks(ctx context.Context) (int, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	const q = `
		SELECT COUNT(*) FROM project_memory_chunks
		WHERE COALESCE(content_class, '') IN ('', 'unclassified')`
	var n int
	if err := r.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountUnclassifiedChunks: %w", err)
	}
	return n, nil
}

// ListUnclassifiedChunksAcrossProjects returns up to limit
// unclassified chunks across all projects, oldest-first. Mirrors
// ListChunksMissingTitle in shape so the auto-backfill loop is a
// drop-in parallel of the titler's. limit ≤ 0 → 100, capped at 1000.
func (r *Repository) ListUnclassifiedChunksAcrossProjects(ctx context.Context, limit int) ([]ClassifyBackfillRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `
SELECT id, project_id,
       COALESCE(source_name, ''),
       COALESCE(producer_role, ''),
       content
FROM project_memory_chunks
WHERE COALESCE(content_class, '') IN ('', 'unclassified')
ORDER BY created_at ASC
LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("ListUnclassifiedChunksAcrossProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ClassifyBackfillRow
	for rows.Next() {
		var row ClassifyBackfillRow
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.SourceName, &row.ProducerRole, &row.Content); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateChunkClass sets content_class on one chunk, recomputing
// expires_at from the new class's TTL (NULL when ttl==0). Used by
// the LLM classifier backfill — distinct from PatchPolicyByArtifact
// because the backfill works per-chunk (not per-artifact) and only
// touches class + expiry, not the other policy columns.
func (r *Repository) UpdateChunkClass(ctx context.Context, chunkID, newClass string, ttl time.Duration) error {
	if r == nil || r.db == nil || chunkID == "" || newClass == "" {
		return nil
	}
	const q = `
UPDATE project_memory_chunks
SET content_class = $2,
    expires_at    = CASE WHEN $3::bigint > 0
                         THEN NOW() + ($3::bigint || ' seconds')::interval
                         ELSE NULL
                    END
WHERE id = $1`
	_, err := r.db.ExecContext(ctx, q, chunkID, newClass, int64(ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("UpdateChunkClass: %w", err)
	}
	return nil
}

// CountUnclassifiedByRole groups chunks whose content_class is
// 'unclassified' (or empty) by producer_role, returning the per-role
// count. Used by `vornikctl memory reclassify` to preview how many
// chunks each role would shift to its derived class before any UPDATE
// runs. NULL/empty producer_role lands under the empty-string key so
// the caller can surface "chunks without a role to reclassify by".
func (r *Repository) CountUnclassifiedByRole(ctx context.Context, projectID string) (map[string]int, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	const q = `
SELECT COALESCE(producer_role, '') AS role, COUNT(*) AS n
FROM project_memory_chunks
WHERE project_id = $1
  AND COALESCE(content_class, '') IN ('', 'unclassified')
GROUP BY COALESCE(producer_role, '')`
	rows, err := r.db.QueryContext(ctx, q, projectID)
	if err != nil {
		return nil, fmt.Errorf("CountUnclassifiedByRole: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int)
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out[role] = n
	}
	return out, rows.Err()
}

// ReclassifyUnclassifiedByRoles flips content_class (and, where the
// new class carries a TTL, expires_at) for every chunk whose producer
// role is in `roles` and whose current class is 'unclassified' (or
// empty). One round-trip per class — the caller groups roles by their
// target class before calling. ttl=0 sets expires_at=NULL (no expiry
// for this class). Returns the count of rows updated.
//
// Bounded by projectID + roles so a misconfigured caller can't sweep
// every chunk in the deployment.
func (r *Repository) ReclassifyUnclassifiedByRoles(
	ctx context.Context,
	projectID, newClass string,
	roles []string,
	ttl time.Duration,
) (int, error) {
	if r == nil || r.db == nil || projectID == "" || newClass == "" || len(roles) == 0 {
		return 0, nil
	}
	const q = `
UPDATE project_memory_chunks
SET content_class = $2,
    expires_at    = CASE WHEN $4::bigint > 0
                         THEN NOW() + ($4::bigint || ' seconds')::interval
                         ELSE NULL
                    END
WHERE project_id = $1
  AND COALESCE(content_class, '') IN ('', 'unclassified')
  AND producer_role = ANY($3::text[])`
	res, err := r.db.ExecContext(ctx, q, projectID, newClass, pq.Array(roles), int64(ttl.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("ReclassifyUnclassifiedByRoles: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ChunkExistsByHash reports whether a chunk with the given content
// hash already exists for the project. Used by the Pipeline's
// dedup gate.
func (r *Repository) ChunkExistsByHash(ctx context.Context, projectID, contentHash string) (bool, error) {
	if r == nil || r.db == nil {
		return false, nil
	}
	if projectID == "" || contentHash == "" {
		return false, nil
	}
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM project_memory_chunks
			WHERE project_id = $1 AND content_hash = $2
		)
	`, projectID, contentHash).Scan(&exists)
	return exists, err
}

// PatchPolicyByArtifact sets the per-policy columns on every chunk
// the indexer just wrote for one artifact. Used by the Pipeline
// to stamp content_class / confidence / producer_role /
// ingest_execution_id / expires_at on chunks that landed via the
// existing IngestText path with schema defaults. Patching by
// artifact_id reaches all the chunks in the artifact (one
// IngestText call typically produces N chunks).
//
// Sets validation_status='unverified' on the patched chunks so
// they emerge from the pipeline with the expected policy state
// — overrides the indexer's schema-default 'unverified' explicitly
// so future schema changes can't silently drift the meaning.
// scopeFilterSQL returns the WHERE-clause snippet that gates a
// search by repo_scope (migration 75). paramIdx is the 1-based
// SQL placeholder index the calling query has assigned to the
// repo_scope parameter (e.g. 7 → "$7").
//
// strict=false is the migration-window default: any of empty
// filter, exact match, '*' cross-cutting, OR uncategorized NULL
// chunks pass. This preserves legacy visibility for chunks
// ingested before migration 75; the bulk-retag CLI (B-6) is the
// path operators use to promote them out of NULL.
//
// strict=true drops the IS NULL fallthrough. Operator-facing
// surfaces (the /ui/memory scope picker) set this so a deliberate
// scope choice actually narrows the result set — "I picked
// repo-X and I want chunks tagged repo-X, not every legacy NULL
// chunk leaking through".
//
// Lifted into one place because five queries share the clause and
// drift between them would be invisible. The unit test
// TestScopeFilterSQL_StrictVsLenient pins both forms.
func scopeFilterSQL(strict bool, paramIdx int) string {
	if strict {
		return fmt.Sprintf("AND ($%d::text IS NULL OR repo_scope = $%d OR repo_scope = '*')", paramIdx, paramIdx)
	}
	return fmt.Sprintf("AND ($%d::text IS NULL OR repo_scope = $%d OR repo_scope = '*' OR repo_scope IS NULL)", paramIdx, paramIdx)
}

// PatchScopeByArtifact stamps repo_scope (migration 75) on every
// chunk under one project_id + artifact_id. Used by the executor's
// post-IngestText hook for workflow-produced chunks where the scope
// arrives via the task payload rather than from the candidate (the
// pipeline.go path).
func (r *Repository) PatchScopeByArtifact(ctx context.Context, projectID, artifactID, repoScope string) error {
	if r == nil || r.db == nil {
		return nil
	}
	if projectID == "" || artifactID == "" || repoScope == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET repo_scope = $3
		WHERE project_id = $1 AND artifact_id = $2
	`, projectID, artifactID, repoScope)
	return err
}

// RepoScopeCount is one row of the scope inventory: the scope
// token (or empty for uncategorized) and how many chunks are
// tagged with it. Mirrors the shape `vornikctl memory scope list`
// emits so the UI dropdown and the CLI surface a consistent
// view of "what scopes does this project have content under".
type RepoScopeCount struct {
	Scope  string // empty string for uncategorized (repo_scope IS NULL)
	Chunks int
}

// ListRepoScopes returns the distinct repo_scope values in a
// project's memory, with chunk counts, sorted by count descending.
// NULL repo_scopes collapse into a single row with Scope="".
//
// Used by the /ui/memory scope picker (B-6) and could back a
// future CLI re-implementation that talks to the daemon rather
// than the DB directly. Bounded: a project tagging hundreds of
// scopes would be operator error, but the query has no LIMIT —
// scope cardinality is naturally small (one per active repo).
func (r *Repository) ListRepoScopes(ctx context.Context, projectID string) ([]RepoScopeCount, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if projectID == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(repo_scope, '') AS scope, COUNT(*) AS n
		FROM project_memory_chunks
		WHERE project_id = $1
		GROUP BY repo_scope
		ORDER BY n DESC, scope ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("memory: list repo scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RepoScopeCount
	for rows.Next() {
		var rsc RepoScopeCount
		if err := rows.Scan(&rsc.Scope, &rsc.Chunks); err != nil {
			return nil, fmt.Errorf("memory: scan scope row: %w", err)
		}
		out = append(out, rsc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: scope rows: %w", err)
	}
	return out, nil
}

func (r *Repository) PatchPolicyByArtifact(ctx context.Context, projectID, artifactID, contentClass string, confidence float32, producerRole, ingestExecutionID string, expiresAt *time.Time, repoScope string) error {
	if r == nil || r.db == nil {
		return nil
	}
	if projectID == "" || artifactID == "" {
		return nil
	}
	// repo_scope uses the NULLIF-COALESCE idiom on a *non-NULL string*
	// parameter so the caller passes "" to mean "leave alone" rather
	// than threading a nullable type. The candidate's RepoScope is
	// the bound source here; empty = uncategorized, "*" = cross-cutting.
	_, err := r.db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		SET content_class       = COALESCE(NULLIF($3, ''), content_class),
		    confidence          = $4,
		    producer_role       = COALESCE(NULLIF($5, ''), producer_role),
		    ingest_execution_id = COALESCE(NULLIF($6, ''), ingest_execution_id),
		    expires_at          = $7,
		    repo_scope          = COALESCE(NULLIF($8, ''), repo_scope),
		    -- A chunk only flips legacy → unverified here; chunks already
		    -- 'verified' (Phase 4) are not downgraded.
		    validation_status   = CASE
		        WHEN validation_status = 'legacy' THEN 'unverified'
		        ELSE validation_status
		    END
		WHERE project_id = $1 AND artifact_id = $2
	`, projectID, artifactID, contentClass, confidence, producerRole, ingestExecutionID, expiresAt, repoScope)
	return err
}

// vectorLiteral converts []float32 into the pgvector text literal format.
func vectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// HybridSearch executes the combined semantic + keyword search using
// Reciprocal Rank Fusion (RRF). RRF combines the rank positions from
// semantic and keyword retrievers rather than their raw scores, which
// prevents cosine-similarity values (0-1) from dominating ts_rank
// values (typically 0.01-0.1). k=60 is the standard RRF constant.
//
// Critically, a chunk that has no embedding yet (embedding IS NULL,
// e.g. freshly ingested content) can still win via its keyword rank
// alone — the previous linear-score formula let semantic results
// with a 0.7 weight beat strong keyword matches at 0.3 weight.
//
// Falls back to KeywordSearch when pgvector is unavailable or queryVec is nil.
// HybridSearch is the legacy unscoped hybrid (semantic + keyword)
// search. NO repo_scope filter — only reachable from
// hybridSearchTemporal's short-circuit path when ALL THREE inputs
// (fromDate, toDate, repoScope) are zero, i.e. when no scope was
// requested. Callers that want scope filtering MUST go through
// HybridSearchWithEpochs (which threads repoScope+strictScope all
// the way down). Verified 2026-05-29 audit: no live call path
// reaches this function with a non-empty scope request — the
// missing scope filter is by design, not a leak.
func (r *Repository) HybridSearch(ctx context.Context, projectID string, queryVec []float32, queryText string, limit int) ([]SearchResult, error) {
	if !r.pgvectorAvailable(ctx) || len(queryVec) == 0 {
		return r.KeywordSearch(ctx, projectID, queryText, limit)
	}

	vecLit := vectorLiteral(queryVec)
	// utility_score boost is multiplicative: base RRF * (1 + utility).
	// utility ∈ [0,1] from UtilityScorer's per-project normalisation,
	// so the maximum boost is 2×. Chunks the corpus actively uses
	// climb the ranking without overwhelming raw relevance signal.
	query := `
WITH semantic AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> $4::vector) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND embedding IS NOT NULL
      AND (expires_at IS NULL OR expires_at > NOW())
    ORDER BY embedding <=> $4::vector LIMIT 20
),
keyword AS (
    SELECT id, row_number() OVER (ORDER BY ts_rank(tsv, plainto_tsquery('vornik_english', $2)) DESC) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND tsv @@ plainto_tsquery('vornik_english', $2)
      AND (expires_at IS NULL OR expires_at > NOW())
    LIMIT 20
)
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       (COALESCE(1.0/(60 + s.rank), 0) + COALESCE(1.0/(60 + k.rank), 0))
           * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at
FROM project_memory_chunks c
LEFT JOIN semantic s ON s.id = c.id
LEFT JOIN keyword  k ON k.id = c.id
WHERE s.id IS NOT NULL OR k.id IS NOT NULL
ORDER BY score DESC LIMIT $3`

	rows, err := r.db.QueryContext(ctx, query, projectID, queryText, limit, vecLit)
	if err != nil {
		// Degrade gracefully — pgvector ops may fail if the extension was
		// removed after startup detection.
		return r.KeywordSearch(ctx, projectID, queryText, limit)
	}
	defer func() { _ = rows.Close() }()

	return scanSearchResults(rows)
}

// hybridSearchTemporal is the no-epoch sibling of HybridSearchWithEpochs;
// it shares the repo_scope filter contract (migration 75) on $7.
func (r *Repository) hybridSearchTemporal(ctx context.Context, projectID string, queryVec []float32, queryText string, limit int, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	if fromDate.IsZero() && toDate.IsZero() && repoScope == "" {
		return r.HybridSearch(ctx, projectID, queryVec, queryText, limit)
	}
	if !r.pgvectorAvailable(ctx) || len(queryVec) == 0 {
		return r.keywordSearchTemporal(ctx, projectID, queryText, limit, fromDate, toDate, repoScope, strictScope)
	}

	vecLit := vectorLiteral(queryVec)
	scopeClause := scopeFilterSQL(strictScope, 7)
	query := fmt.Sprintf(`
WITH semantic AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> $4::vector) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND embedding IS NOT NULL
      AND (expires_at IS NULL OR expires_at > NOW())
      AND ($5::timestamptz IS NULL OR created_at >= $5::timestamptz)
      AND ($6::timestamptz IS NULL OR created_at <= $6::timestamptz)
      %[1]s
    ORDER BY embedding <=> $4::vector LIMIT 20
),
keyword AS (
    SELECT id, row_number() OVER (ORDER BY ts_rank(tsv, plainto_tsquery('vornik_english', $2)) DESC) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND tsv @@ plainto_tsquery('vornik_english', $2)
      AND (expires_at IS NULL OR expires_at > NOW())
      AND ($5::timestamptz IS NULL OR created_at >= $5::timestamptz)
      AND ($6::timestamptz IS NULL OR created_at <= $6::timestamptz)
      %[1]s
    LIMIT 20
)
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       (COALESCE(1.0/(60 + s.rank), 0) + COALESCE(1.0/(60 + k.rank), 0))
           * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at,
       COALESCE(c.repo_scope, '') AS repo_scope
FROM project_memory_chunks c
LEFT JOIN semantic s ON s.id = c.id
LEFT JOIN keyword  k ON k.id = c.id
WHERE s.id IS NOT NULL OR k.id IS NOT NULL
ORDER BY score DESC LIMIT $3`, scopeClause)

	rows, err := r.db.QueryContext(ctx, query, projectID, queryText, limit, vecLit, nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return r.keywordSearchTemporal(ctx, projectID, queryText, limit, fromDate, toDate, repoScope, strictScope)
	}
	defer func() { _ = rows.Close() }()

	return scanSearchResults(rows)
}

// HybridSearchWithEpochs is the Phase-3 epoch-aware search. When
// epochsEnabled is false, behaves exactly like HybridSearch
// (backwards-compatible). When true, the WHERE clause adds:
//
//	AND (c.epoch_id IS NULL OR c.epoch_id = ANY($epochs))
//	AND  c.lifecycle_state = 'published'
//	AND  c.validation_status NOT IN ('refuted','superseded')
//
// epoch_id IS NULL keeps legacy chunks (Phase 0 backfill) visible
// indefinitely until operators retire them. The lifecycle +
// validation filter excludes work-in-progress and rolled-back
// chunks.
func (r *Repository) HybridSearchWithEpochs(ctx context.Context, projectID string, queryVec []float32, queryText string, limit int, activeEpochs []string, epochsEnabled bool, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	if !epochsEnabled {
		return r.hybridSearchTemporal(ctx, projectID, queryVec, queryText, limit, fromDate, toDate, repoScope, strictScope)
	}
	if !r.pgvectorAvailable(ctx) || len(queryVec) == 0 {
		return r.keywordSearchWithEpochsTemporal(ctx, projectID, queryText, limit, activeEpochs, fromDate, toDate, repoScope, strictScope)
	}

	vecLit := vectorLiteral(queryVec)
	// $5 is a TEXT[] of active epoch IDs. The epoch_id IS NULL
	// branch survives the cardinality=0 case (no active epochs)
	// so a never-published-pipeline project still returns its
	// legacy chunks. RRF (k=60) replaces the former linear score
	// combination for the same reasons as HybridSearch above.
	//
	// $6 / $7 are temporal lower/upper bounds (created_at);
	// passing a zero time.Time disables the corresponding side
	// because $6::timestamptz IS NULL becomes true when zero is
	// rendered as Postgres NULL by lib/pq's parameter binding.
	//
	// $8 is the repo_scope filter (migration 75). scopeFilterSQL
	// emits the canonical "$8 IS NULL OR exact match OR '*'" clause,
	// optionally extended with the legacy "OR IS NULL" leak-through.
	// See its doc for the strict-vs-lenient contract.
	scopeClause := scopeFilterSQL(strictScope, 8)
	query := fmt.Sprintf(`
WITH semantic AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> $4::vector) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND embedding IS NOT NULL
      AND lifecycle_state = 'published'
      AND validation_status NOT IN ('refuted','superseded')
      AND (expires_at IS NULL OR expires_at > NOW())
      AND (epoch_id IS NULL OR epoch_id = ANY($5::text[]))
      AND ($6::timestamptz IS NULL OR created_at >= $6::timestamptz)
      AND ($7::timestamptz IS NULL OR created_at <= $7::timestamptz)
      %[1]s
    ORDER BY embedding <=> $4::vector LIMIT 20
),
keyword AS (
    SELECT id, row_number() OVER (ORDER BY ts_rank(tsv, plainto_tsquery('vornik_english', $2)) DESC) AS rank
    FROM project_memory_chunks
    WHERE project_id = $1 AND tsv @@ plainto_tsquery('vornik_english', $2)
      AND lifecycle_state = 'published'
      AND validation_status NOT IN ('refuted','superseded')
      AND (expires_at IS NULL OR expires_at > NOW())
      AND (epoch_id IS NULL OR epoch_id = ANY($5::text[]))
      AND ($6::timestamptz IS NULL OR created_at >= $6::timestamptz)
      AND ($7::timestamptz IS NULL OR created_at <= $7::timestamptz)
      %[1]s
    LIMIT 20
)
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       (COALESCE(1.0/(60 + s.rank), 0) + COALESCE(1.0/(60 + k.rank), 0))
           * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at,
       COALESCE(c.repo_scope, '') AS repo_scope
FROM project_memory_chunks c
LEFT JOIN semantic s ON s.id = c.id
LEFT JOIN keyword  k ON k.id = c.id
WHERE s.id IS NOT NULL OR k.id IS NOT NULL
ORDER BY score DESC LIMIT $3`, scopeClause)

	rows, err := r.db.QueryContext(ctx, query, projectID, queryText, limit, vecLit, pqStringArray(activeEpochs), nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return r.keywordSearchWithEpochsTemporal(ctx, projectID, queryText, limit, activeEpochs, fromDate, toDate, repoScope, strictScope)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

// nullableTime returns nil for a zero time.Time so the
// driver binds it as SQL NULL — that's what the
// `$N::timestamptz IS NULL OR …` guard expects. Non-zero
// values pass through unchanged.
func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

// keywordSearchWithEpochs is the FTS-only variant when pgvector
// is unavailable or the query produced no embedding. Preserved
// without temporal filters for the legacy callsite; the temporal
// variant lives in keywordSearchWithEpochsTemporal below.
func (r *Repository) keywordSearchWithEpochs(ctx context.Context, projectID, queryText string, limit int, activeEpochs []string) ([]SearchResult, error) {
	// Legacy shim — no scope filter and lenient strict mode (the
	// IS NULL leak-through is irrelevant here since repoScope is
	// empty anyway, so the first OR catches everything).
	return r.keywordSearchWithEpochsTemporal(ctx, projectID, queryText, limit, activeEpochs, time.Time{}, time.Time{}, "", false)
}

func (r *Repository) keywordSearchTemporal(ctx context.Context, projectID, queryText string, limit int, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	q := fmt.Sprintf(`
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       ts_rank(tsv, plainto_tsquery('vornik_english', $2)) * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at,
       COALESCE(c.repo_scope, '') AS repo_scope
FROM project_memory_chunks c
WHERE c.project_id = $1 AND c.tsv @@ plainto_tsquery('vornik_english', $2)
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
  AND ($4::timestamptz IS NULL OR c.created_at >= $4::timestamptz)
  AND ($5::timestamptz IS NULL OR c.created_at <= $5::timestamptz)
  %s
ORDER BY score DESC LIMIT $3`, strings.ReplaceAll(scopeFilterSQL(strictScope, 6), "repo_scope", "c.repo_scope"))
	rows, err := r.db.QueryContext(ctx, q, projectID, queryText, limit, nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return r.substringSearchTemporal(ctx, projectID, queryText, limit, fromDate, toDate, repoScope, strictScope)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

// keywordSearchWithEpochsTemporal extends the FTS fallback with
// the same created_at lower/upper bounds the hybrid query uses.
// Zero time.Time on either side disables that bound — see
// nullableTime() for the NULL-binding contract.
func (r *Repository) keywordSearchWithEpochsTemporal(ctx context.Context, projectID, queryText string, limit int, activeEpochs []string, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	q := fmt.Sprintf(`
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       ts_rank(tsv, plainto_tsquery('vornik_english', $2)) * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at,
       COALESCE(c.repo_scope, '') AS repo_scope
FROM project_memory_chunks c
WHERE c.project_id = $1 AND c.tsv @@ plainto_tsquery('vornik_english', $2)
  AND c.lifecycle_state = 'published'
  AND c.validation_status NOT IN ('refuted','superseded')
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
  AND (c.epoch_id IS NULL OR c.epoch_id = ANY($4::text[]))
  AND ($5::timestamptz IS NULL OR c.created_at >= $5::timestamptz)
  AND ($6::timestamptz IS NULL OR c.created_at <= $6::timestamptz)
  %s
ORDER BY score DESC LIMIT $3`, strings.ReplaceAll(scopeFilterSQL(strictScope, 7), "repo_scope", "c.repo_scope"))
	rows, err := r.db.QueryContext(ctx, q, projectID, queryText, limit, pqStringArray(activeEpochs), nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return r.substringSearchWithEpochsTemporal(ctx, projectID, queryText, limit, activeEpochs, fromDate, toDate, repoScope, strictScope)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

// pqStringArray adapts []string for the pq driver. nil/empty
// becomes an empty Postgres array '{}' so the IS NULL OR =ANY()
// clause still works (ANY against empty array is FALSE — only the
// IS NULL leg survives, exactly what we want for "no active epochs").
func pqStringArray(s []string) interface{} {
	if s == nil {
		s = []string{}
	}
	return pq.Array(s)
}

// KeywordSearch performs a full-text search using PostgreSQL tsvector.
// Filters out chunks whose TTL has lapsed; the lifecycle/validation
// filters are deliberately left off this legacy path (the
// epoch-aware variant adds them).
//
// 2026.7.0 F7 — three-tier resilience: when the tsvector query
// errors (extension dropped, malformed query, generated-column
// drift), fall through to substringSearch so the caller never
// sees "memory unavailable" for a transient infra hiccup. The
// substring path is a coarser ranker but it's strictly readable
// — a SELECT … WHERE content ILIKE that needs no extension and
// no special index. Drives the SaaS reliability SLA.
func (r *Repository) KeywordSearch(ctx context.Context, projectID string, queryText string, limit int) ([]SearchResult, error) {
	const q = `
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       ts_rank(tsv, plainto_tsquery('vornik_english', $2)) * (1 + COALESCE(c.utility_score, 0)) AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at
FROM project_memory_chunks c
WHERE project_id = $1 AND tsv @@ plainto_tsquery('vornik_english', $2)
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
ORDER BY score DESC LIMIT $3`

	rows, err := r.db.QueryContext(ctx, q, projectID, queryText, limit)
	if err != nil {
		// Legacy KeywordSearch is unscoped (no repo_scope filter
		// in its own query) — preserve that by passing empty
		// repoScope + lenient strictScope. Callers needing scope
		// use the keywordSearchTemporal / *Epochs* variants.
		return r.substringSearch(ctx, projectID, queryText, limit, "", false)
	}
	defer func() { _ = rows.Close() }()

	return scanSearchResults(rows)
}

// RecentChunkRow is the projection ListRecentChunks returns. Carries
// enough metadata for the SessionStart digest (LLD 22 Phase 2) to
// render "here's what this project recently learned" without a
// second round-trip per row. Companion-origin rows are detectable
// via SourceName ("companion:<client_kind>:...") per LLD 22.
type RecentChunkRow struct {
	ChunkID      string
	TaskID       string
	SourceName   string
	ContentClass string
	Content      string
	CreatedAt    time.Time

	// RepoScope is the chunk's repo-scope token (migration 75).
	// Empty string surfaces a NULL-scoped chunk so clients see the
	// migration-grace leak surface. 2026-05-28.
	RepoScope string

	// HasEmbedding flags whether the embedding column is populated.
	// The async embed Worker fills it ~5s after ingest; until then
	// recall's vector half can't score this chunk. Lets the
	// companion adapter derive the "ready / pending_embedding"
	// ingest_status field exposed to MCP clients.
	HasEmbedding bool

	// PolicyWarning is populated by Searcher.RecentWithContext under
	// EnforcementAdvisory when the firewall would have blocked this
	// chunk: "<decision>: <reason>". Empty when the chunk is Allowed,
	// when the firewall is off, or when ListRecentChunksWithOptions is
	// called directly (no firewall pass). Mirrors SearchResult.PolicyWarning
	// so the companion digest exposes the same advisory signal recall does.
	PolicyWarning string
}

// ListRecentChunks is the legacy non-strict variant — preserved so
// the existing SessionStart digest call site doesn't need
// touching. Defaults to non-strict scope (NULL chunks included).
// 2026-05-28: new ListRecentChunksWithOptions is the strict-aware
// surface; this wrapper preserves backward compatibility.
func (r *Repository) ListRecentChunks(ctx context.Context, projectID string, limit int, repoScope string) ([]RecentChunkRow, error) {
	return r.ListRecentChunksWithOptions(ctx, projectID, limit, repoScope, false, false)
}

// ListRecentChunksWithOptions returns up to limit chunks for a
// project, newest-first, with the metadata the companion
// `recent_memory` MCP tool surfaces. TTL-aware (matches the
// search-path expires_at guard). limit ≤ 0 → 5; values > 50 are
// capped to keep the SessionStart digest small.
//
// strictScope drops the migration-grace `OR repo_scope IS NULL`
// clause when true AND repoScope is non-empty. Without this,
// every NULL-scoped chunk surfaces under every scope filter —
// the leak the 2026-05-28 investigation caught.
//
// Each row carries RepoScope + HasEmbedding so the companion
// adapter can derive ingest_status + show the chunk's true
// scope to the MCP client.
func (r *Repository) ListRecentChunksWithOptions(ctx context.Context, projectID string, limit int, repoScope string, strictScope, onlyUntagged bool) ([]RecentChunkRow, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	// Scope filter. onlyUntagged isolates the NULL-scoped ("untagged")
	// bucket that ListRepoScopes reports under the empty-string key, so
	// the operator can actually enumerate what `vornikctl memory scope
	// retag` would touch (the leak-triage gap from the 2026-05-30
	// report — recent_memory has no query vector so it isn't blocked by
	// the embedding path). It takes precedence over repoScope/strict.
	// Otherwise: migration-75 OR-of-(match | '*' [| NULL]) shape, same
	// as the searcher's hot path; strict drops the OR-NULL fallthrough.
	scopeClause := "AND ($3::text IS NULL OR repo_scope = $3 OR repo_scope = '*' OR repo_scope IS NULL)"
	queryArgs := []any{projectID, limit, nullableString(repoScope)}
	switch {
	case onlyUntagged:
		scopeClause = "AND repo_scope IS NULL"
		queryArgs = []any{projectID, limit} // $3 unreferenced — don't bind it
	case strictScope:
		scopeClause = "AND ($3::text IS NULL OR repo_scope = $3 OR repo_scope = '*')"
	}
	q := `
SELECT id,
       COALESCE(task_id, ''),
       COALESCE(source_name, ''),
       COALESCE(content_class, ''),
       content,
       created_at,
       COALESCE(repo_scope, ''),
       (embedding IS NOT NULL) AS has_embedding
FROM project_memory_chunks
WHERE project_id = $1
  AND (expires_at IS NULL OR expires_at > NOW())
  ` + scopeClause + `
ORDER BY created_at DESC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("ListRecentChunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]RecentChunkRow, 0, limit)
	for rows.Next() {
		var row RecentChunkRow
		if err := rows.Scan(&row.ChunkID, &row.TaskID, &row.SourceName, &row.ContentClass, &row.Content, &row.CreatedAt, &row.RepoScope, &row.HasEmbedding); err != nil {
			return nil, fmt.Errorf("scan recent chunk: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent chunks: %w", err)
	}
	return out, nil
}

// ListChunkContents returns up to limit chunk contents for a
// project, newest first. Used by the 2026.7.0 F8 consolidator to
// build a project-wide term-frequency gist without paying per-
// chunk LLM cost. TTL-aware (same `expires_at` guard the search
// paths apply); limit≤0 falls back to 1000 to bound memory use.
func (r *Repository) ListChunkContents(ctx context.Context, projectID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	const q = `
SELECT content
FROM project_memory_chunks
WHERE project_id = $1
  AND (expires_at IS NULL OR expires_at > NOW())
ORDER BY created_at DESC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list chunk contents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]string, 0, limit)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan chunk content: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chunk contents: %w", err)
	}
	return out, nil
}

// substringSearch is the tier-3 fallback when both pgvector and
// tsvector are unavailable. Plain ILIKE over the content column
// — coarser ranking (no rank function, just lexical sort) but
// guaranteed to work as long as the table is readable. Caller
// gets a SearchResult slice with a fixed score of 1.0 so the
// downstream RRF / reranker code paths still work, but the
// score isn't semantically meaningful — the values mostly mean
// "we found something at all". Surfaces a "degraded" content_class
// so audit logs can spot fallback fires in retrospect.
// All three substringSearch* functions accept repoScope +
// strictScope (added 2026-05-29 after audit-agent finding):
// pre-fix the FTS tier applied the scope filter but the ILIKE
// fallback dropped it — under a transient tsvector / pgvector
// outage every scoped recall returned chunks from all scopes,
// a silent cross-scope leak. The scope filter is now threaded
// through the cascade and uses the same scopeFilterSQL helper
// as the upper tiers.

// escapeLikeWildcards makes a user-supplied string safe to embed in a
// LIKE/ILIKE pattern: backslash, % and _ become literals (paired with
// ESCAPE '\' on the query). Without this a search query containing % or
// _ is interpreted as a wildcard — e.g. "_" or "%" would match nearly
// every row (wildcard injection in the tier-3 substring fallback).
func escapeLikeWildcards(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *Repository) substringSearch(ctx context.Context, projectID, queryText string, limit int, repoScope string, strictScope bool) ([]SearchResult, error) {
	q := strings.TrimSpace(queryText)
	if q == "" {
		// An empty query under ILIKE matches every row — refuse so
		// the cascade can't accidentally dump the whole table.
		return nil, nil
	}
	sql := fmt.Sprintf(`
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       1.0 AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at
FROM project_memory_chunks c
WHERE c.project_id = $1 AND c.content ILIKE '%%' || $2 || '%%' ESCAPE '\'
  AND c.lifecycle_state = 'published'
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
  %s
ORDER BY c.created_at DESC LIMIT $3`, strings.ReplaceAll(scopeFilterSQL(strictScope, 4), "repo_scope", "c.repo_scope"))
	rows, err := r.db.QueryContext(ctx, sql, projectID, escapeLikeWildcards(q), limit, nullableString(repoScope))
	if err != nil {
		// True end of the line — the DB itself is unreachable.
		// Return the error rather than a third nested fallback;
		// the searcher's audit will log the bubble-up.
		return nil, fmt.Errorf("substring search (tier-3 fallback): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

func (r *Repository) substringSearchTemporal(ctx context.Context, projectID, queryText string, limit int, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	q := strings.TrimSpace(queryText)
	if q == "" {
		return nil, nil
	}
	sql := fmt.Sprintf(`
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       1.0 AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at
FROM project_memory_chunks c
WHERE c.project_id = $1 AND c.content ILIKE '%%' || $2 || '%%' ESCAPE '\'
  AND c.lifecycle_state = 'published'
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
  AND ($4::timestamptz IS NULL OR c.created_at >= $4::timestamptz)
  AND ($5::timestamptz IS NULL OR c.created_at <= $5::timestamptz)
  %s
ORDER BY c.created_at DESC LIMIT $3`, strings.ReplaceAll(scopeFilterSQL(strictScope, 6), "repo_scope", "c.repo_scope"))
	rows, err := r.db.QueryContext(ctx, sql, projectID, escapeLikeWildcards(q), limit, nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return nil, fmt.Errorf("substring search temporal (tier-3 fallback): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

func (r *Repository) substringSearchWithEpochsTemporal(ctx context.Context, projectID, queryText string, limit int, activeEpochs []string, fromDate, toDate time.Time, repoScope string, strictScope bool) ([]SearchResult, error) {
	q := strings.TrimSpace(queryText)
	if q == "" {
		return nil, nil
	}
	sql := fmt.Sprintf(`
SELECT c.id, c.project_id, COALESCE(c.task_id,''), c.source_name, c.content,
       1.0 AS score,
       COALESCE(c.content_class, ''),
       c.is_alive, c.last_checked_at
FROM project_memory_chunks c
WHERE c.project_id = $1 AND c.content ILIKE '%%' || $2 || '%%' ESCAPE '\'
  AND c.lifecycle_state = 'published'
  AND c.validation_status NOT IN ('refuted','superseded')
  AND (c.expires_at IS NULL OR c.expires_at > NOW())
  AND (c.epoch_id IS NULL OR c.epoch_id = ANY($4::text[]))
  AND ($5::timestamptz IS NULL OR c.created_at >= $5::timestamptz)
  AND ($6::timestamptz IS NULL OR c.created_at <= $6::timestamptz)
  %s
ORDER BY c.created_at DESC LIMIT $3`, strings.ReplaceAll(scopeFilterSQL(strictScope, 7), "repo_scope", "c.repo_scope"))
	rows, err := r.db.QueryContext(ctx, sql, projectID, escapeLikeWildcards(q), limit, pqStringArray(activeEpochs), nullableTime(fromDate), nullableTime(toDate), nullableString(repoScope))
	if err != nil {
		return nil, fmt.Errorf("substring search epochs temporal (tier-3 fallback): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanSearchResults(rows)
}

// MemoryState holds snapshot counts used for gauge metrics.
type MemoryState struct {
	ProjectID   string
	ChunksTotal int64
	QueueDepth  int64
}

// DLQEntry is one row from memory_embed_dlq. Powers
// `vornikctl memory dlq` read paths.
type DLQEntry struct {
	ChunkID       string
	ProjectID     string
	Reason        string
	LastError     string
	RetryCount    int
	RetryAfter    time.Time
	FirstFailedAt time.Time
	LastFailedAt  time.Time
}

// DLQMove shifts a single chunk from the embed queue into the DLQ.
// reason is the coarse class (embedding_failed / dimension_mismatch /
// store_failed); lastError is the underlying message. retryAfter is
// computed by the caller — typically worker.processBatch.
//
// Deletes from memory_embed_queue in the same transaction so a row
// never appears in both places (the worker would otherwise re-fetch
// it on the next tick). Upserts into the DLQ so repeated failures for
// the same chunk bump retry_count and extend last_failed_at without
// creating duplicate rows.
func (r *Repository) DLQMove(ctx context.Context, chunkID, projectID, reason, lastError string, retryAfter time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dlq begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_embed_queue WHERE chunk_id = $1`, chunkID,
	); err != nil {
		return fmt.Errorf("dlq delete queue row: %w", err)
	}
	// retry_after on the UPDATE branch is computed server-side
	// using the post-bump retry_count so the exponential backoff
	// honours the chunk's actual retry history. Pre-fix the
	// worker always passed dlqBackoff(0) = 10m on the UPDATE
	// path, flat-rating a persistently broken endpoint regardless
	// of how many prior attempts had failed. Formula matches
	// worker.go:dlqBackoff exactly: 10m * 2^retry_count, capped
	// at 24h. INSERT branch (first failure) uses the caller's
	// retry_after which is already dlqBackoff(0) = 10m.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO memory_embed_dlq (chunk_id, project_id, reason, last_error, retry_count, retry_after, first_failed_at, last_failed_at)
VALUES ($1, $2, $3, $4, 0, $5, now(), now())
ON CONFLICT (chunk_id) DO UPDATE SET
    reason         = EXCLUDED.reason,
    last_error     = EXCLUDED.last_error,
    retry_count    = memory_embed_dlq.retry_count + 1,
    retry_after    = now() + LEAST(
        INTERVAL '24 hours',
        INTERVAL '10 minutes' * power(2, memory_embed_dlq.retry_count + 1)::int
    ),
    last_failed_at = now()
`, chunkID, projectID, reason, lastError, retryAfter); err != nil {
		return fmt.Errorf("dlq upsert: %w", err)
	}
	return tx.Commit()
}

// DLQPark marks a DLQ row as never-auto-retry (retry_count = -1). For
// permanent failure classes (dimension mismatch, content too large)
// where auto-retry would just burn cycles.
func (r *Repository) DLQPark(ctx context.Context, chunkID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE memory_embed_dlq SET retry_count = -1, last_failed_at = now() WHERE chunk_id = $1`,
		chunkID,
	)
	return err
}

// DLQReadyForRetry fetches chunks whose retry_after is past and whose
// retry_count is non-negative (i.e. auto-retryable). Caller replays
// them by re-inserting into memory_embed_queue. Limit caps the batch
// so one slow replay doesn't hold the lock too long.
func (r *Repository) DLQReadyForRetry(ctx context.Context, limit int) ([]DLQEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT chunk_id, project_id, reason, last_error, retry_count, retry_after, first_failed_at, last_failed_at
FROM memory_embed_dlq
WHERE retry_count >= 0 AND retry_after <= now()
ORDER BY retry_after ASC
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("dlq list ready: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDLQEntries(rows)
}

// DLQList returns DLQ rows matching the optional projectID, newest
// failure first. Used by `vornikctl memory dlq`.
func (r *Repository) DLQList(ctx context.Context, projectID string, limit int) ([]DLQEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `
SELECT chunk_id, project_id, reason, last_error, retry_count, retry_after, first_failed_at, last_failed_at
FROM memory_embed_dlq
`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = $1"
		args = append(args, projectID)
	}
	query += " ORDER BY last_failed_at DESC LIMIT "
	if projectID != "" {
		query += "$2"
		args = append(args, limit)
	} else {
		query += "$1"
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dlq list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanDLQEntries(rows)
}

// DLQReplay moves a DLQ row back into memory_embed_queue and deletes
// it from the DLQ. Used by worker auto-retry (when retry_after lapses
// naturally) and by the operator CLI for manual replay.
func (r *Repository) DLQReplay(ctx context.Context, chunkIDs []string) (int, error) {
	if len(chunkIDs) == 0 {
		return 0, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("dlq replay begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-insert into the embed queue. ON CONFLICT is a no-op because a
	// chunk can only ever be in one place — if it's already queued, the
	// DLQ row is stale and we just drop it.
	placeholders := make([]string, len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)*2)
	for i, id := range chunkIDs {
		placeholders[i] = fmt.Sprintf("($%d, (SELECT project_id FROM memory_embed_dlq WHERE chunk_id = $%d))",
			2*i+1, 2*i+2)
		args = append(args, id, id)
	}
	insertQuery := `INSERT INTO memory_embed_queue (chunk_id, project_id) VALUES ` +
		strings.Join(placeholders, ", ") +
		` ON CONFLICT (chunk_id) DO NOTHING`
	if _, err := tx.ExecContext(ctx, insertQuery, args...); err != nil {
		return 0, fmt.Errorf("dlq replay insert: %w", err)
	}

	// Delete from DLQ.
	deleteQuery := `DELETE FROM memory_embed_dlq WHERE chunk_id = ANY($1)`
	res, err := tx.ExecContext(ctx, deleteQuery, pq.Array(chunkIDs))
	if err != nil {
		return 0, fmt.Errorf("dlq replay delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("dlq replay commit: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanDLQEntries(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]DLQEntry, error) {
	var out []DLQEntry
	for rows.Next() {
		var e DLQEntry
		if err := rows.Scan(&e.ChunkID, &e.ProjectID, &e.Reason, &e.LastError,
			&e.RetryCount, &e.RetryAfter, &e.FirstFailedAt, &e.LastFailedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ProjectMemoryStats extends MemoryState with embedding coverage.
// Exposed via GET /api/v1/memory/stats for operator introspection and
// consumed by vornikctl memory stats. The extra filtered count lets
// operators see "N chunks still unembedded" at a glance — the value
// QueryState was too narrow to surface.
type ProjectMemoryStats struct {
	ProjectID      string `json:"projectId"`
	ChunksTotal    int64  `json:"chunksTotal"`
	ChunksEmbedded int64  `json:"chunksEmbedded"`
	QueueDepth     int64  `json:"queueDepth"`
}

// Stats returns per-project chunk totals, embedded counts, and queue
// depth in one round-trip. The FILTER aggregate requires Postgres 9.4+
// which vornik has required since inception.
func (r *Repository) Stats(ctx context.Context) ([]ProjectMemoryStats, error) {
	const q = `
SELECT c.project_id,
       COUNT(c.id)                                       AS chunks_total,
       COUNT(c.id) FILTER (WHERE c.embedding IS NOT NULL) AS chunks_embedded,
       COUNT(q.chunk_id)                                 AS queue_depth
FROM project_memory_chunks c
LEFT JOIN memory_embed_queue q ON q.chunk_id = c.id
GROUP BY c.project_id
ORDER BY c.project_id`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query memory stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProjectMemoryStats
	for rows.Next() {
		var s ProjectMemoryStats
		if err := rows.Scan(&s.ProjectID, &s.ChunksTotal, &s.ChunksEmbedded, &s.QueueDepth); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryState returns per-project chunk counts and queue depth.
func (r *Repository) QueryState(ctx context.Context) ([]MemoryState, error) {
	const q = `
SELECT c.project_id,
       COUNT(c.id) AS chunks_total,
       COUNT(q.chunk_id) AS queue_depth
FROM project_memory_chunks c
LEFT JOIN memory_embed_queue q ON q.chunk_id = c.id
GROUP BY c.project_id`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query memory state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var states []MemoryState
	for rows.Next() {
		var s MemoryState
		if err := rows.Scan(&s.ProjectID, &s.ChunksTotal, &s.QueueDepth); err != nil {
			return nil, err
		}
		states = append(states, s)
	}
	return states, rows.Err()
}

// scanSearchResults scans rows from either hybrid or keyword search queries.
// Accepts three column shapes for forward/backward compatibility across
// mixed-version deployments:
//   - 6 cols: legacy (no content_class, no liveness)
//   - 7 cols: + content_class
//   - 9 cols: + content_class + is_alive + last_checked_at (2026-05-17)
//
// Older Repository binaries hitting newer SQL (or vice versa) keep
// working without crashing.
func scanSearchResults(rows *sql.Rows) ([]SearchResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	// Column tiers (each strictly more-permissive than the one
	// below it; the loop picks the highest that matches):
	//   10 cols → +repo_scope        (post-B-6-followup; UI surface)
	//    9 cols → +liveness / class
	//    7 cols → +class only
	//    6 cols → bare legacy result
	withRepoScope := len(cols) >= 10
	withLiveness := len(cols) >= 9
	withClass := len(cols) >= 7
	var results []SearchResult
	for rows.Next() {
		var sr SearchResult
		switch {
		case withRepoScope:
			var class sql.NullString
			var alive sql.NullBool
			var checked sql.NullTime
			var scope sql.NullString
			if err := rows.Scan(
				&sr.ChunkID, &sr.ProjectID, &sr.TaskID,
				&sr.SourceName, &sr.Content, &sr.Score, &class,
				&alive, &checked, &scope,
			); err != nil {
				return nil, err
			}
			if class.Valid {
				sr.ContentClass = class.String
			}
			if alive.Valid {
				v := alive.Bool
				sr.IsAlive = &v
			}
			if checked.Valid {
				t := checked.Time
				sr.LastCheckedAt = &t
			}
			if scope.Valid {
				sr.RepoScope = scope.String
			}
		case withLiveness:
			var class sql.NullString
			var alive sql.NullBool
			var checked sql.NullTime
			if err := rows.Scan(
				&sr.ChunkID, &sr.ProjectID, &sr.TaskID,
				&sr.SourceName, &sr.Content, &sr.Score, &class,
				&alive, &checked,
			); err != nil {
				return nil, err
			}
			if class.Valid {
				sr.ContentClass = class.String
			}
			if alive.Valid {
				v := alive.Bool
				sr.IsAlive = &v
			}
			if checked.Valid {
				t := checked.Time
				sr.LastCheckedAt = &t
			}
		case withClass:
			var class sql.NullString
			if err := rows.Scan(
				&sr.ChunkID, &sr.ProjectID, &sr.TaskID,
				&sr.SourceName, &sr.Content, &sr.Score, &class,
			); err != nil {
				return nil, err
			}
			if class.Valid {
				sr.ContentClass = class.String
			}
		default:
			if err := rows.Scan(
				&sr.ChunkID, &sr.ProjectID, &sr.TaskID,
				&sr.SourceName, &sr.Content, &sr.Score,
			); err != nil {
				return nil, err
			}
		}
		results = append(results, sr)
	}
	return results, rows.Err()
}
