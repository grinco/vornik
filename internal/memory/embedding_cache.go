package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// EmbedCache is the narrow interface the Embedder consumes to skip
// the upstream API call when a content+model pair has already been
// embedded. Production wires *embeddingCacheRepo (backed by the
// embedding_cache postgres table from migration 41); tests inject
// an in-memory fake. Nil EmbedCache disables the cache — Embed
// behaves exactly as before.
//
// Cache lookup is keyed on (content_hash, model) because
// embeddings are model-bound: switching from
// text-embedding-3-small to text-embedding-3-large produces
// different vectors for the same text.
//
// Errors from Get / Put are logged-and-ignored at the call site
// (the embedder degrades gracefully — a broken cache should never
// stall ingestion). The interface signatures still return errors
// so the impl can surface a transient DB hiccup.
type EmbedCache interface {
	// Get returns the cached vector + true when a hit exists for
	// (hash, model). Returns nil + false on miss with err == nil;
	// returns nil + false on storage error with err != nil so the
	// caller can log without retrying.
	Get(ctx context.Context, hash, model string) ([]float32, bool, error)
	// Put stores the vector for the (hash, model) key. Upsert
	// semantics: a second call with the same key just refreshes
	// last_hit_at + embedding bytes. No-op when vec is empty.
	Put(ctx context.Context, hash, model string, vec []float32) error
}

// ContentHash is the canonical hashing function used as the
// embedding cache key. SHA-256 over the raw text bytes — fast,
// deterministic, collision-free for our scale. Exported so
// retention sweepers / debug tooling can independently verify
// what a given text would hash to.
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// embeddingCacheRepo is the postgres-backed EmbedCache implementation.
// Wraps *Repository's *sql.DB; doesn't pull in any new dependencies.
type embeddingCacheRepo struct {
	db *sql.DB
}

// NewEmbeddingCache constructs a postgres-backed EmbedCache. Pass
// the same *sql.DB the rest of the memory subsystem uses;
// migration 41 (`create_embedding_cache`) wires the schema.
func NewEmbeddingCache(db *sql.DB) EmbedCache {
	if db == nil {
		return nil
	}
	return &embeddingCacheRepo{db: db}
}

// EmbeddingCacheStats summarises the embedding_cache table for the
// /ui/spend dashboard panel. RowCount is the literal SELECT
// COUNT(*); ApproxBytes is the on-disk table size (pg_total_
// relation_size including indexes) so the operator can see how
// much disk the cache costs.
type EmbeddingCacheStats struct {
	RowCount    int64
	ApproxBytes int64
	// DistinctModels is the number of unique model IDs present in
	// the cache. Useful for spotting "did the embedder swap models
	// and produce two parallel caches?" without scanning the
	// catalog by hand.
	DistinctModels int
}

// CacheStats returns row count + on-disk size + distinct-model
// count for the embedding_cache table. Safe to call from the UI
// hot path — the COUNT(*) hits the primary key, pg_total_
// relation_size is constant-time. Returns zero values + nil
// error when the table is absent (older deployments where
// migration 41 hasn't been applied yet) so the spend panel can
// render an "embedding cache disabled" placeholder without 500ing.
func (r *embeddingCacheRepo) CacheStats(ctx context.Context) (EmbeddingCacheStats, error) {
	if r == nil || r.db == nil {
		return EmbeddingCacheStats{}, nil
	}
	var stats EmbeddingCacheStats
	// to_regclass returns NULL when the table doesn't exist; this
	// guards against "older deployment, migration 41 not applied"
	// without raising a noisy "relation does not exist" error.
	var present bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT to_regclass('public.embedding_cache') IS NOT NULL`).Scan(&present); err != nil {
		return EmbeddingCacheStats{}, err
	}
	if !present {
		return stats, nil
	}
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT model) FROM embedding_cache`).
		Scan(&stats.RowCount, &stats.DistinctModels); err != nil {
		return stats, err
	}
	// pg_total_relation_size includes TOAST + indexes; the vector
	// column dominates the per-row size so this is the metric
	// that matches "how much disk am I spending on this."
	_ = r.db.QueryRowContext(ctx,
		`SELECT pg_total_relation_size('public.embedding_cache')`).Scan(&stats.ApproxBytes)
	return stats, nil
}

// Get returns the cached vector for (hash, model). Updates
// last_hit_at on every hit so a future LRU eviction has the
// right ordering. The update is best-effort — a failed UPDATE
// is logged via the caller's path; the cached vector is still
// returned.
func (r *embeddingCacheRepo) Get(ctx context.Context, hash, model string) ([]float32, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, nil
	}
	if hash == "" || model == "" {
		return nil, false, nil
	}
	var lit string
	err := r.db.QueryRowContext(ctx,
		`SELECT embedding::text FROM embedding_cache WHERE content_hash = $1 AND model = $2`,
		hash, model,
	).Scan(&lit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("embedding cache get: %w", err)
	}
	vec := parseVectorLiteral(lit)
	if len(vec) == 0 {
		return nil, false, nil
	}
	// Best-effort touch — ignore errors so the cache stays useful
	// even if the bookkeeping write fails.
	_, _ = r.db.ExecContext(ctx,
		`UPDATE embedding_cache SET last_hit_at = NOW() WHERE content_hash = $1 AND model = $2`,
		hash, model,
	)
	return vec, true, nil
}

// Put upserts the embedding for (hash, model). Identical rows
// just refresh last_hit_at via the ON CONFLICT clause.
func (r *embeddingCacheRepo) Put(ctx context.Context, hash, model string, vec []float32) error {
	if r == nil || r.db == nil {
		return nil
	}
	if hash == "" || model == "" || len(vec) == 0 {
		return nil
	}
	lit := vectorLiteral(vec)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO embedding_cache (content_hash, model, embedding, created_at, last_hit_at)
		VALUES ($1, $2, $3::vector, NOW(), NOW())
		ON CONFLICT (content_hash, model) DO UPDATE SET
		    embedding   = EXCLUDED.embedding,
		    last_hit_at = NOW()
	`, hash, model, lit)
	if err != nil {
		return fmt.Errorf("embedding cache put: %w", err)
	}
	return nil
}
