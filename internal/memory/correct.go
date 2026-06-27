// Memory correction surface — gives operators a way to flag wrong
// claims that have leaked into the corpus and replace them with
// authoritative corrections. Companion to the existing supersession
// + quarantine paths that fire automatically on ingest; this layer
// is operator-driven, invoked via the dispatcher's memory_correct
// tool or the vornikctl audit CLI.
//
// Workflow:
//
//  1. Operator (in chat) says "fact X is wrong, it's actually Y".
//  2. Dispatcher LLM calls memory_correct(wrong="...", correction="...").
//  3. Corrector.RefuteByClaim runs a hybrid search for the wrong
//     claim and flips matching chunks to validation_status='refuted'.
//     The retrieval layer already excludes refuted rows from
//     memory_search / memory_recall (see HybridSearch in
//     repository.go) so the next CV generation skips them.
//  4. Corrector.InsertCorrection writes a fresh chunk with
//     validation_status='verified', content_class='decision', and
//     producer_role='operator_correction' so it ranks
//     authoritatively in subsequent retrievals.
//  5. (Optional) the dispatcher enqueues a follow-up adaptive task
//     that asks the researcher to re-derive the fact from the
//     original source — closes the loop on contaminated corpora.

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// correctorSearcher is the narrow read surface Corrector needs.
// Defined as an interface so production wires the concrete
// *Searcher while tests can inject a stub that returns canned
// hits — *Searcher's New() takes a DB + embedder + config, more
// machinery than a unit test should own.
type correctorSearcher interface {
	Search(ctx context.Context, projectID, query string, limit int) ([]SearchResult, error)
}

// Corrector wires the read (search) + write (refute, insert)
// halves of the correction flow. Constructed by Manager.New (see
// NewCorrector below). Nil-safe — callers receiving a nil
// Corrector get error returns rather than panics.
type Corrector struct {
	Repo     *Repository
	Searcher correctorSearcher
}

// NewCorrector binds an existing repo + searcher into the
// correction surface. Production wires it from Manager.Repository()
// + Manager.Searcher; tests construct directly with a stub
// searcher.
func NewCorrector(repo *Repository, searcher correctorSearcher) *Corrector {
	return &Corrector{Repo: repo, Searcher: searcher}
}

// RefutedChunk is the per-row record returned by RefuteByClaim
// so the dispatcher tool can surface "I refuted these chunks"
// back to the operator without a second DB round-trip.
type RefutedChunk struct {
	ID         string
	SourceName string
	Preview    string  // first 200 chars of the refuted content
	Score      float64 // hybrid score from the search — kept so the LLM can decide whether to escalate
}

// RefuteByClaim searches the project's corpus for chunks similar
// to `wrongClaim`, marks the top-N (bounded by maxRefutes) as
// validation_status='refuted', and returns the records the
// caller can preview back to the operator. Cap is enforced — a
// runaway LLM that calls with maxRefutes=1000 still only flips
// at most 20 rows per call.
//
// Returns ([], nil) when no matches are found OR memory is
// disabled — callers treat "no matches" as a no-op rather than
// an error.
func (c *Corrector) RefuteByClaim(ctx context.Context, projectID, wrongClaim string, maxRefutes int) ([]RefutedChunk, error) {
	if c == nil || c.Repo == nil || c.Searcher == nil {
		return nil, fmt.Errorf("memory corrector: not configured")
	}
	if projectID == "" || wrongClaim == "" {
		return nil, fmt.Errorf("memory corrector: project id and wrongClaim required")
	}
	if maxRefutes <= 0 {
		maxRefutes = 3
	}
	if maxRefutes > 20 {
		maxRefutes = 20
	}

	hits, err := c.Searcher.Search(ctx, projectID, wrongClaim, maxRefutes)
	if err != nil {
		return nil, fmt.Errorf("memory corrector: search: %w", err)
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	if _, err := c.Repo.MarkRefutedByIDs(ctx, projectID, ids); err != nil {
		return nil, fmt.Errorf("memory corrector: mark refuted: %w", err)
	}
	out := make([]RefutedChunk, 0, len(hits))
	for _, h := range hits {
		preview := h.Content
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		out = append(out, RefutedChunk{
			ID:         h.ChunkID,
			SourceName: h.SourceName,
			Preview:    preview,
			Score:      h.Score,
		})
	}
	return out, nil
}

// RefuteByIDs soft-refutes specific chunks by ID — the surgical
// counterpart to RefuteByClaim. RefuteByClaim demotes whatever a
// hybrid search ranks highest for a claim, which is the wrong tool
// when the operator already knows the exact stale chunk AND
// authoritative corrections rank above it (refuting "top matches"
// would then demote the corrections, not the stale chunk). This path
// flips exactly the named IDs to validation_status='refuted' and
// touches nothing else. Project-scoped + idempotent via
// MarkRefutedByIDs (already-refuted/superseded or wrong-project IDs
// are skipped). Returns the count actually flipped.
func (c *Corrector) RefuteByIDs(ctx context.Context, projectID string, chunkIDs []string) (int, error) {
	if c == nil || c.Repo == nil {
		return 0, fmt.Errorf("memory corrector: not configured")
	}
	if projectID == "" || len(chunkIDs) == 0 {
		return 0, fmt.Errorf("memory corrector: project id and at least one chunk id required")
	}
	return c.Repo.MarkRefutedByIDs(ctx, projectID, chunkIDs)
}

// HardEvict permanently deletes the named chunks from the project's
// memory store, cascading through embed queues + entity mentions
// and writing a per-chunk audit-tombstone row. Use cases: GDPR-
// style "forget this" requests and cleanup of confirmed-bad records
// that the soft-refute path leaves cluttering the search index.
//
// Soft-refute is still the right default for "this record is wrong,
// demote it in search" — hard-evict is for "this record should not
// have existed at all." The two surfaces co-exist; pick by use case.
//
// Returns the audit rows for the chunks actually deleted (may be
// shorter than chunkIDs if some IDs were stale / wrong-project) so
// the caller can surface "I evicted these" back to the operator.
// reason + evictedBy land in the memory_eviction_audit row — pass
// non-empty values; the audit table is the GDPR compliance hook.
func (c *Corrector) HardEvict(ctx context.Context, projectID string, chunkIDs []string, reason, evictedBy string) ([]EvictionAuditRow, error) {
	if c == nil || c.Repo == nil {
		return nil, fmt.Errorf("memory corrector: not configured")
	}
	if projectID == "" {
		return nil, fmt.Errorf("memory corrector: project id required")
	}
	if len(chunkIDs) == 0 {
		return nil, nil
	}
	return c.Repo.HardEvict(ctx, projectID, chunkIDs, reason, evictedBy)
}

// InsertCorrection writes a fresh memory chunk carrying the
// operator's correction. The chunk lands as:
//
//   - validation_status = 'verified' (so it survives the refuted-
//     row filter on retrieval)
//   - content_class     = 'decision'
//   - producer_role     = 'operator_correction'
//   - confidence        = 0.95
//   - source_name       = "operator_correction_<RFC3339>"
//
// Returns the new chunk's ID. The embed worker picks the row up
// on its next tick and fills in the vector embedding async — the
// chunk is searchable via FTS immediately, vector-searchable
// after the next worker pass.
// InsertCorrection accepts a repoScope parameter (added
// 2026-05-29 audit fix): pre-fix every operator correction
// landed with NULL repo_scope, making it invisible to strict-
// scope searches the operator's own queries used. Empty scope
// preserves the legacy NULL behaviour for callers that haven't
// adopted the parameter yet.
func (c *Corrector) InsertCorrection(ctx context.Context, projectID, content, repoScope string) (string, error) {
	if c == nil || c.Repo == nil {
		return "", fmt.Errorf("memory corrector: not configured")
	}
	if projectID == "" || content == "" {
		return "", fmt.Errorf("memory corrector: project id and content required")
	}
	// Prepend a timestamped header so two corrections with
	// identical body still produce distinct content_hash values
	// — otherwise the (project_id, content_hash) uniqueness
	// constraint would reject the second one as a duplicate.
	header := fmt.Sprintf("[operator correction @ %s]\n\n", time.Now().UTC().Format(time.RFC3339Nano))
	body := header + content
	hash := sha256.Sum256([]byte(body))
	chunkID := GenerateChunkID()
	sourceName := "operator_correction_" + time.Now().UTC().Format("20060102T150405Z")

	if err := c.Repo.insertOperatorCorrection(ctx, &operatorCorrectionRow{
		ID:          chunkID,
		ProjectID:   projectID,
		SourceName:  sourceName,
		Content:     body,
		ContentHash: hex.EncodeToString(hash[:]),
		RepoScope:   repoScope,
	}); err != nil {
		return "", fmt.Errorf("memory corrector: insert: %w", err)
	}
	// Queue the chunk for embedding so vector search picks it up
	// on the next worker tick. FTS search already includes it
	// immediately because the FTS index is updated at INSERT
	// time by the existing trigger.
	if err := c.Repo.EnqueueForEmbedding(ctx, []string{chunkID}); err != nil {
		// Non-fatal: the chunk is still in the corpus, just not
		// vector-indexed. Operators retrieving via FTS still see it.
		return chunkID, fmt.Errorf("memory corrector: enqueue embed (chunk still inserted): %w", err)
	}
	return chunkID, nil
}

// GenerateChunkID returns a fresh chunk ID using the package-wide
// ID helper. Exposed so the Corrector + future call sites can
// mint IDs without re-implementing the shape.
func GenerateChunkID() string {
	return persistence.GenerateID("chunk")
}
