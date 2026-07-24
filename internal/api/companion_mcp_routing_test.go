package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Confidence-based retrieval routing (P3) — companion `recall` surface.

// routingCompanion implements BOTH MemoryCompanionAdapter and
// MemoryRoutingCompanionAdapter so companionToolRecall takes the routing path.
type routingCompanion struct {
	fakeMemoryCompanion
	verdict *RoutingVerdictWire
	hits    []MemorySearchResult
}

func (r *routingCompanion) RecallRouting(_ context.Context, _, _ string, _ RecallOptions) ([]MemorySearchResult, *RoutingVerdictWire, error) {
	return r.hits, r.verdict, nil
}

func TestCompanionMCP_Recall_RoutingOn_EmitsVerdict(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &routingCompanion{
		hits: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "decision/x",
				Content: "an aged decision", Score: 0.9,
				ContentClass: "decision", Confidence: 0.9, ValidationStatus: "verified"},
		},
		verdict: &RoutingVerdictWire{
			Verdict: "medium", Guidance: "re-confirm it is still current", WidenRounds: 0,
			Basis: RoutingVerdictBasisWire{ResultCount: 1, TrustMean: 0.84, AgeCapped: true, WeakestDim: "aged_decision"},
		},
	}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "planning fact", "limit": 5},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "routing recall must not flag IsError: %s", text)
	var out recallResult
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "medium", out.RetrievalTrustVerdict)
	assert.Equal(t, "re-confirm it is still current", out.Guidance)
	require.NotNil(t, out.VerdictBasis)
	assert.Equal(t, "aged_decision", out.VerdictBasis.WeakestDim)
	assert.True(t, out.VerdictBasis.AgeCapped)
	require.Len(t, out.Hits, 1)
	assert.Equal(t, 0.9, out.Hits[0].Confidence)
	assert.Equal(t, "verified", out.Hits[0].ValidationStatus)
}

// Byte-identical when the adapter is NOT routing-capable: the recall result
// must carry no routing keys and no per-hit trust fields.
func TestCompanionMCP_Recall_RoutingOff_ByteIdentical(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{
			{ChunkID: "ck1", ProjectID: "alpha", SourceName: "research/r", Content: "hit", Score: 0.9},
		},
	}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, false)

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "recall",
		"arguments": map[string]any{"query": "x", "limit": 5},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	for _, k := range []string{"retrieval_trust_verdict", "guidance", "verdict_basis", "confidence", "validation_status"} {
		assert.NotContains(t, text, k, "routing-off recall must omit %q", k)
	}
	assert.True(t, strings.Contains(text, `"hits"`))
}
