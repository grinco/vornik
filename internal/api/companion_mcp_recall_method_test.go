package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory"
)

// `recall` reports the retrieval path it actually took.
//
// Until now the response described the results and not the path, so a caller
// could not tell an RRF ordering from a reranked one. That gap has a price tag:
// three `--tier2-only` benchmark runs on 2026-08-12 — a mode documented as
// needing "no reranker, nothing billable" — each billed ten cloud reranker calls
// and returned three different rankings of a byte-identical corpus. The
// determinism gate refused all three pairings, and the cause could only be
// established afterwards, by hand, from the usage ledger.

// observingCompanion writes into the observation bag the way the real searcher
// does, so these tests exercise the ctx plumbing rather than mocking it away.
type observingCompanion struct {
	fakeMemoryCompanion
	reranked  bool
	attempted bool
	sawBag    bool
}

func (o *observingCompanion) Recall(ctx context.Context, _, _ string, _ RecallOptions) ([]MemorySearchResult, error) {
	if obs := memory.RetrievalObservationFromContext(ctx); obs != nil {
		o.sawBag = true
		obs.Reranked = o.reranked
		obs.RerankAttempted = o.attempted
	}
	return o.recallReturn, nil
}

func recallOnce(t *testing.T, adapter MemoryCompanionAdapter) recallResult {
	t.Helper()
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = adapter
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "x", "limit": 5},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "recall must not flag IsError: %s", text)
	var out recallResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

func TestRecall_ReportsRerankedMethod(t *testing.T) {
	adapter := &observingCompanion{
		fakeMemoryCompanion: fakeMemoryCompanion{recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "research/r", Content: "hit", Score: 0.9},
		}},
		reranked: true, attempted: true,
	}
	out := recallOnce(t, adapter)

	require.True(t, adapter.sawBag,
		"the observation bag never reached the adapter, so nothing downstream could report the path")
	assert.Equal(t, "context-assembly+rerank", out.RetrievalMethod)
}

func TestRecall_ReportsUnrerankedMethod(t *testing.T) {
	adapter := &observingCompanion{
		fakeMemoryCompanion: fakeMemoryCompanion{recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "research/r", Content: "hit", Score: 0.9},
		}},
	}
	out := recallOnce(t, adapter)
	assert.Equal(t, "context-assembly", out.RetrievalMethod)
}

// TestRecall_AttemptedButFailedRerankIsNotReranked is the mixture case that makes
// this worth reporting at all. A deployment with the reranker enabled, wired and
// requested still returns RRF order when the call blows its 8s deadline — 45 of
// 400 queries in the 2026-08-14 baseline. Describing those as reranked would let
// a gate believe it measured a path it did not.
func TestRecall_AttemptedButFailedRerankIsNotReranked(t *testing.T) {
	adapter := &observingCompanion{
		fakeMemoryCompanion: fakeMemoryCompanion{recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "research/r", Content: "hit", Score: 0.9},
		}},
		reranked: false, attempted: true,
	}
	out := recallOnce(t, adapter)
	assert.Equal(t, "context-assembly", out.RetrievalMethod,
		"a rerank that lost its deadline left the caller holding RRF order")
}

// TestRecall_MethodIsAlwaysPresent is the central decision, and the one this
// whole arc keeps re-learning: absence must not be readable as a value.
//
// With `omitempty`, an unreranked run and a daemon too old to report would both
// send nothing, and a client could not distinguish "verified: no rerank" from
// "no idea". A consumer that treats the missing field as "not reranked" then has
// a guard that passes precisely when it learned nothing — the shape of the
// write-target guard that compared two strings the operator typed.
func TestRecall_MethodIsAlwaysPresent(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{recallReturn: []MemorySearchResult{
		{ChunkID: "ck1", ProjectID: "alpha", SourceName: "research/r", Content: "hit", Score: 0.9},
	}}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "x", "limit": 5},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var raw2 map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &raw2))

	got, ok := raw2["retrieval_method"]
	require.True(t, ok,
		"retrieval_method must be present on EVERY recall response. Omitting it when "+
			"unreranked makes 'no rerank happened' indistinguishable from 'this daemon "+
			"cannot say', and a client cannot fail closed on a distinction it cannot see")
	assert.Equal(t, "context-assembly", got)
}

// statsProvider is a MemoryStatsProvider for the whoami readiness fields.
type statsProvider struct{ rows []MemoryProjectStats }

func (s statsProvider) Stats(context.Context) ([]MemoryProjectStats, error) { return s.rows, nil }

// TestWhoami_ReportsEmbeddingReadiness pins these fields to the WHOAMI response.
//
// Twice now a field intended for whoami has been added to a neighbouring handler's
// map instead — the `database` field on 2026-08-12 morning, and readiness the same
// afternoon, which landed in companionToolCatalog. Both compiled, both shipped, and
// both were absent from the response the caller actually reads. A test that asserts
// the field is present in whoami's own payload is the only thing that catches it.
func TestWhoami_ReportsEmbeddingReadiness(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryStats = statsProvider{rows: []MemoryProjectStats{
		{ProjectID: "alpha", ChunksTotal: 200, ChunksEmbedded: 50, QueueDepth: 150},
		{ProjectID: "other", ChunksTotal: 10, ChunksEmbedded: 10},
	}}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "whoami must not flag IsError: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	got, ok := out["embedding_readiness"]
	require.True(t, ok, "whoami does not report embedding_readiness. The benchmark "+
		"harness reads it from HERE, because /api/v1/memory/stats is admin-only and its "+
		"companion key gets 403 — which is why a partially-embedded corpus was scored "+
		"as though it were settled")
	assert.Equal(t, 0.25, got, "readiness must be THIS key's project, not a total")
	assert.Equal(t, float64(150), out["memory_embed_queue_depth"],
		"queue depth is the signal a settling caller waits on")
	assert.Equal(t, float64(50), out["memory_chunks_embedded"])
	assert.Equal(t, float64(200), out["memory_chunks_total"])
}

// TestWhoami_OmitsReadinessWhenStatsAreUnavailable: absent must mean "cannot say".
// Defaulting to 1.0 would tell a settling caller the corpus is ready when nothing
// checked; defaulting to 0.0 would stall it for ever on a deployment with no stats.
func TestWhoami_OmitsReadinessWhenStatsAreUnavailable(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryStats = nil
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	if _, present := out["embedding_readiness"]; present {
		t.Error("readiness reported with no stats provider wired; absence is the only " +
			"honest answer, and the harness refuses on it by design")
	}
}

// TestWhoami_EmptyCorpusIsReady: a project with no chunks is vacuously settled.
// Reporting 0.0 would stall a caller waiting for a fresh database to warm up.
func TestWhoami_EmptyCorpusIsReady(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryStats = statsProvider{rows: []MemoryProjectStats{{ProjectID: "alpha"}}}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, _ := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, 1.0, out["embedding_readiness"], "an empty corpus has nothing unembedded")
}
