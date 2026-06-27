package service

// Coverage-gap sweep (2026-06-18), Tier 2. Unit coverage for the pure
// converters and nil-safe guards in memory_adapter.go. The DB-backed
// adapter methods (Recall/Search/RecentMemory over a concrete
// memory.Searcher) are exercised in the api/blackbox integration lanes
// — here we pin the no-DB surface: field-mapping fidelity, the ingest-
// status decision table, the embedding-cache stats adapter (the
// untested twin of responseCacheStatsAdapter), and the defensive nil
// paths that keep the daemon's degraded mode panic-free.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
)

func TestDeriveIngestStatus(t *testing.T) {
	cases := []struct {
		name         string
		hasEmbedding bool
		contentClass string
		want         string
	}{
		{"no embedding yet", false, "decision", api.IngestStatusPendingEmbedding},
		{"no embedding overrides empty class", false, "", api.IngestStatusPendingEmbedding},
		{"embedded but unclassified", true, "unclassified", api.IngestStatusPendingClassification},
		{"embedded but empty class", true, "", api.IngestStatusPendingClassification},
		{"embedded and classified", true, "decision", api.IngestStatusReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deriveIngestStatus(tc.hasEmbedding, tc.contentClass))
		})
	}
}

// TestChunkPolicyRow_RoundTrip guards against a field being dropped in
// one direction of the api<->memory translation: a full round trip must
// be lossless. Adding a field to ChunkPolicyRow without wiring both
// converters fails here.
func TestChunkPolicyRow_RoundTrip(t *testing.T) {
	orig := api.ChunkPolicyRow{
		ChunkID:            "chunk-1",
		TenantID:           "tenant-1",
		SensitivityTier:    "restricted",
		ProvenanceSource:   "companion:claude-code",
		ProvenanceProducer: "operator",
		ProvenanceTrust:    3,
		ProvenanceURL:      "https://example.test/doc",
		FirewallExpiresAt:  func() *time.Time { ts := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC); return &ts }(),
		PermittedRoles:     []string{"lead", "analyst"},
		AllowedPurposes:    []string{"operational"},
		PolicyDigest:       "sha256:abc",
		ContentClass:       "decision",
		ValidationStatus:   "validated",
	}

	got := chunkPolicyRowMemoryToAPI(chunkPolicyRowAPIToMemory(orig))
	assert.Equal(t, orig, got, "api->memory->api round trip must be lossless")
}

// --- embeddingCacheStatsAdapter: the untested twin of responseCacheStatsAdapter ---

type stubEmbedCache struct {
	stats memory.EmbeddingCacheStats
	err   error
}

func (s *stubEmbedCache) Get(_ context.Context, _, _ string) ([]float32, bool, error) {
	return nil, false, nil
}

func (s *stubEmbedCache) Put(_ context.Context, _, _ string, _ []float32) error { return nil }

func (s *stubEmbedCache) CacheStats(_ context.Context) (memory.EmbeddingCacheStats, error) {
	return s.stats, s.err
}

// statslessEmbedCache implements EmbedCache but NOT CacheStats, so the
// constructor's type-assert must fail and return nil.
type statslessEmbedCache struct{}

func (statslessEmbedCache) Get(_ context.Context, _, _ string) ([]float32, bool, error) {
	return nil, false, nil
}
func (statslessEmbedCache) Put(_ context.Context, _, _ string, _ []float32) error { return nil }

func TestNewEmbeddingCacheStatsAdapter_NilSourceReturnsNil(t *testing.T) {
	if got := newEmbeddingCacheStatsAdapter(nil); got != nil {
		t.Errorf("expected nil for nil cache, got %T", got)
	}
}

func TestNewEmbeddingCacheStatsAdapter_NoStatsMethodReturnsNil(t *testing.T) {
	if got := newEmbeddingCacheStatsAdapter(statslessEmbedCache{}); got != nil {
		t.Errorf("expected nil when CacheStats absent, got %T", got)
	}
}

func TestEmbeddingCacheStatsAdapter_PassesThrough(t *testing.T) {
	src := &stubEmbedCache{stats: memory.EmbeddingCacheStats{RowCount: 9, ApproxBytes: 2048, DistinctModels: 2}}
	adapter := newEmbeddingCacheStatsAdapter(src)
	require.NotNil(t, adapter)
	got, err := adapter.CacheStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(9), got.RowCount)
	assert.Equal(t, int64(2048), got.ApproxBytes)
	assert.Equal(t, 2, got.DistinctModels)
}

func TestEmbeddingCacheStatsAdapter_PropagatesError(t *testing.T) {
	adapter := newEmbeddingCacheStatsAdapter(&stubEmbedCache{err: errors.New("db down")})
	require.NotNil(t, adapter)
	_, err := adapter.CacheStats(context.Background())
	require.Error(t, err)
}

func TestEmbeddingCacheStatsAdapter_NilReceiverSafe(t *testing.T) {
	var a *embeddingCacheStatsAdapter
	got, err := a.CacheStats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.RowCount)
}

// --- companion adapter: constructor wiring + defensive nil guards ---

func TestNewMemoryCompanionAdapter_RequiresAllDeps(t *testing.T) {
	// Any missing dependency yields nil so container_http.go skips the
	// half-wired tool surface rather than serving a tool that panics.
	assert.Nil(t, newMemoryCompanionAdapter(nil, &memory.Pipeline{}, &memory.Repository{}))
	assert.Nil(t, newMemoryCompanionAdapter(&memory.Searcher{}, nil, &memory.Repository{}))
	assert.Nil(t, newMemoryCompanionAdapter(&memory.Searcher{}, &memory.Pipeline{}, nil))

	// All three present → a live adapter.
	require.NotNil(t, newMemoryCompanionAdapter(&memory.Searcher{}, &memory.Pipeline{}, &memory.Repository{}))
}

func TestMemoryCompanionAdapter_NilGuardsReturnEmpty(t *testing.T) {
	ctx := context.Background()

	// Zero-value adapter (all deps nil) hits the early-return guard on
	// every method without ever touching the nil Searcher/Pipeline/Repo.
	a := &memoryCompanionAdapter{}

	recall, err := a.Recall(ctx, "proj", "q", api.RecallOptions{})
	require.NoError(t, err)
	assert.Nil(t, recall)

	recent, err := a.RecentMemory(ctx, "proj", 10, "", false, false, "", "")
	require.NoError(t, err)
	assert.Nil(t, recent)

	scopes, err := a.ListRepoScopes(ctx, "proj")
	require.NoError(t, err)
	assert.Nil(t, scopes)

	res, err := a.Remember(ctx, api.RememberInput{ProjectID: "proj", Content: "x"})
	require.NoError(t, err)
	assert.Equal(t, api.RememberResult{}, res)
}

func TestMemoryCacheStatsAdapter_NilManagerDisabled(t *testing.T) {
	ctx := context.Background()
	a := &memoryCacheStatsAdapter{m: nil}

	emb, err := a.EmbeddingCacheStats(ctx)
	require.NoError(t, err)
	assert.False(t, emb.Enabled)

	resp, err := a.ResponseCacheStats(ctx)
	require.NoError(t, err)
	assert.False(t, resp.Enabled)
}
