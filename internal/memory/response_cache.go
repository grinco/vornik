package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/chat"
)

// Purpose tags used as the second key axis on llm_response_cache.
// One per call site so eviction + stats can scope by purpose
// without scanning the whole table, and so a prompt change in one
// caller doesn't accidentally hit a row written by another.
const (
	ResponseCachePurposeTitler     = "memory_titler"
	ResponseCachePurposeClassifier = "memory_classifier"
	ResponseCachePurposeKGExtract  = "memory_kg_extract"
	// ResponseCachePurposeInstinctDistill tags the instinct layer's
	// cheap-model distillation pass (internal/instinct). Identical
	// ambiguous clusters short-circuit the upstream LLM call so a
	// re-scanned window never re-pays for the same generalisation.
	ResponseCachePurposeInstinctDistill = "instinct_distill"
)

// ResponseCache is the narrow interface the memory background
// consumers (Titler, Classifier, KG Extractor) consume to skip
// re-paying the upstream LLM call when a (model, purpose, prompt)
// triple has already been answered. Production wires
// *responseCacheRepo (backed by the llm_response_cache postgres
// table from migration 47); tests inject an in-memory fake. Nil
// ResponseCache disables caching — the caller behaves exactly as
// before.
//
// Errors from Get / Put are logged-and-ignored at the call site
// (the caller degrades gracefully — a broken cache should never
// stall ingestion). The interface signatures still return errors
// so the impl can surface a transient DB hiccup for metrics.
type ResponseCache interface {
	// Get returns the cached response content + the original
	// (prompt_tokens, completion_tokens) recorded when the row
	// was first written, plus true on hit. Returns "", 0, 0,
	// false, nil on miss; returns "", 0, 0, false, err on
	// storage error so the caller can log without retrying.
	Get(ctx context.Context, key string) (content string, promptTokens, completionTokens int, hit bool, err error)
	// Put stores response_content keyed on (key). Upsert
	// semantics: a second call with the same key refreshes
	// last_hit_at and increments hit_count.
	Put(ctx context.Context, key, model, purpose, content string, promptTokens, completionTokens int) error
}

// ResponseCacheKey hashes the (model, purpose, messages) triple
// into the canonical cache key. SHA-256 hex over a delimiter-
// separated fingerprint — NUL bytes can't appear in chat content
// (which is JSON-serialised over the wire) so the delimiter is
// collision-safe. Exported so debug tooling can independently
// verify what a given call would hash to.
//
// Cache invalidation on prompt drift: if a system prompt is
// edited in source, the hash changes and the cache misses on
// the next call. No explicit invalidation needed.
func ResponseCacheKey(model, purpose string, messages []chat.Message) string {
	h := sha256.New()
	_, _ = h.Write([]byte(model))
	_ = writeNul(h)
	_, _ = h.Write([]byte(purpose))
	_ = writeNul(h)
	for _, m := range messages {
		_, _ = h.Write([]byte(m.Role))
		_ = writeNul(h)
		_, _ = h.Write([]byte(m.Content))
		_ = writeNul(h)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeNul(w interface{ Write([]byte) (int, error) }) error {
	_, err := w.Write([]byte{0})
	return err
}

// responseCacheRepo is the postgres-backed ResponseCache. Wraps
// the same *sql.DB the rest of the memory subsystem uses; no new
// dependencies.
type responseCacheRepo struct {
	db *sql.DB
	// costFn optionally costs each row at (model, prompt, completion)
	// resolution so CacheStats can populate TotalSavingsUSD. Nil
	// leaves savings at 0 — hit volume stays visible.
	costFn func(model string, promptTokens, completionTokens int) float64
}

// NewResponseCache constructs a postgres-backed ResponseCache.
// Pass the same *sql.DB the rest of the memory subsystem uses;
// migration 47 (`create_llm_response_cache`) wires the schema.
// Returns nil for a nil db so the caller can pass through to a
// "cache disabled" code path without branching.
func NewResponseCache(db *sql.DB) ResponseCache {
	if db == nil {
		return nil
	}
	return &responseCacheRepo{db: db}
}

// NewResponseCacheWithPricing wires a cost function so CacheStats
// surfaces TotalSavingsUSD on the /ui/spend tile. costFn shape
// matches pricing.Table.CostUSD so the service container can pass
// it directly. nil db still returns nil.
func NewResponseCacheWithPricing(db *sql.DB, costFn func(model string, promptTokens, completionTokens int) float64) ResponseCache {
	if db == nil {
		return nil
	}
	return &responseCacheRepo{db: db, costFn: costFn}
}

// ResponseCacheStats summarises the llm_response_cache table for
// the /ui/spend dashboard panel. Mirrors EmbeddingCacheStats shape
// so the panel template can render both with the same partial.
type ResponseCacheStats struct {
	RowCount    int64
	ApproxBytes int64
	// DistinctPurposes is the count of unique purpose tags in the
	// cache. With only three callers wired today the expected
	// value is 0–3.
	DistinctPurposes int
	// TotalHits is the lifetime SUM(hit_count) across all rows —
	// the operator-visible count of LLM calls saved.
	TotalHits int64
	// TotalSavingsUSD is the lifetime dollar amount saved by
	// cache hits, computed as SUM(hit_count × CostUSD(model,
	// prompt_tokens, completion_tokens)) over every row. Zero
	// when no pricing function was wired (an "un-priced model"
	// deployment) — TotalHits still reflects volume.
	TotalSavingsUSD float64
}

// CacheStats returns row count + on-disk size + distinct-purpose
// count + lifetime hit total. Returns zero values + nil error
// when the table is absent (deployments where migration 47
// hasn't run) so the spend panel can render an "enabled?"
// placeholder without 500ing.
func (r *responseCacheRepo) CacheStats(ctx context.Context) (ResponseCacheStats, error) {
	if r == nil || r.db == nil {
		return ResponseCacheStats{}, nil
	}
	var stats ResponseCacheStats
	var present bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT to_regclass('public.llm_response_cache') IS NOT NULL`).Scan(&present); err != nil {
		return ResponseCacheStats{}, err
	}
	if !present {
		return stats, nil
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT purpose), COALESCE(SUM(hit_count), 0)
		 FROM llm_response_cache`).
		Scan(&stats.RowCount, &stats.DistinctPurposes, &stats.TotalHits); err != nil {
		return stats, err
	}
	_ = r.db.QueryRowContext(ctx,
		`SELECT pg_total_relation_size('public.llm_response_cache')`).Scan(&stats.ApproxBytes)

	// Savings math — only when a cost function was wired AND
	// rows exist (no point scanning an empty table). The
	// per-row query keeps memory at O(rows) rather than the
	// SUM(...) shape that would need pricing inside SQL.
	if r.costFn != nil && stats.RowCount > 0 {
		stats.TotalSavingsUSD = r.computeSavings(ctx)
	}
	return stats, nil
}

// computeSavings sums each row's per-hit cost × hit_count using
// the wired cost function. Best-effort: a query error returns 0
// (callers still get RowCount / TotalHits). Rows with hit_count=0
// are skipped at the SQL layer so the table walk doesn't multiply
// by zero on cold rows.
func (r *responseCacheRepo) computeSavings(ctx context.Context) float64 {
	rows, err := r.db.QueryContext(ctx,
		`SELECT model, prompt_tokens, completion_tokens, hit_count
		 FROM llm_response_cache WHERE hit_count > 0`)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()
	var total float64
	for rows.Next() {
		var (
			model      string
			promptTok  int
			completion int
			hits       int64
		)
		if err := rows.Scan(&model, &promptTok, &completion, &hits); err != nil {
			return 0
		}
		total += r.costFn(model, promptTok, completion) * float64(hits)
	}
	return total
}

// Get returns the cached response for (key). Updates last_hit_at
// + increments hit_count on every hit so eviction has the right
// ordering and operators can see hot rows. The update is
// best-effort — a failed UPDATE is silently swallowed; the cached
// content is still returned.
func (r *responseCacheRepo) Get(ctx context.Context, key string) (string, int, int, bool, error) {
	if r == nil || r.db == nil {
		return "", 0, 0, false, nil
	}
	if key == "" {
		return "", 0, 0, false, nil
	}
	var (
		content    string
		promptTok  int
		completion int
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT response_content, prompt_tokens, completion_tokens
		 FROM llm_response_cache WHERE cache_key = $1`,
		key,
	).Scan(&content, &promptTok, &completion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, 0, false, nil
		}
		return "", 0, 0, false, fmt.Errorf("response cache get: %w", err)
	}
	_, _ = r.db.ExecContext(ctx,
		`UPDATE llm_response_cache
		 SET last_hit_at = NOW(), hit_count = hit_count + 1
		 WHERE cache_key = $1`,
		key,
	)
	return content, promptTok, completion, true, nil
}

// Put upserts the cached response. Identical keys refresh
// last_hit_at + content via ON CONFLICT. hit_count is NOT bumped
// on Put — only Get increments it (a re-write isn't a hit).
func (r *responseCacheRepo) Put(ctx context.Context, key, model, purpose, content string, promptTokens, completionTokens int) error {
	if r == nil || r.db == nil {
		return nil
	}
	if key == "" || content == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO llm_response_cache
		    (cache_key, model, purpose, response_content,
		     prompt_tokens, completion_tokens, created_at, last_hit_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT (cache_key) DO UPDATE SET
		    response_content  = EXCLUDED.response_content,
		    prompt_tokens     = EXCLUDED.prompt_tokens,
		    completion_tokens = EXCLUDED.completion_tokens,
		    last_hit_at       = NOW()
	`, key, model, purpose, content, promptTokens, completionTokens)
	if err != nil {
		return fmt.Errorf("response cache put: %w", err)
	}
	return nil
}
