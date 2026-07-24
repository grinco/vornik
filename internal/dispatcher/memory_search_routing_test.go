package dispatcher

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
)

// Confidence-based retrieval routing (P3) — dispatcher memory_search surface.

// routingMemory implements BOTH MemorySearcher and MemoryRoutingSearcher so
// the memory_search tool takes the routing path (banner + per-hit tokens).
type routingMemory struct {
	stubMemory
	verdict    *memory.RoutingVerdict
	routingRes []memory.SearchResult
	gotRouting bool
}

func (r *routingMemory) RecallWithRouting(_ context.Context, _, _ string, opts memory.SearchOptions, _ memoryfirewall.RequestContext) ([]memory.SearchResult, *memory.RoutingVerdict, error) {
	r.gotRouting = opts.Routing
	return r.routingRes, r.verdict, nil
}

func TestMemorySearch_RoutingOn_BannerAndPerHitTokens(t *testing.T) {
	mem := &routingMemory{
		routingRes: []memory.SearchResult{
			{ChunkID: "c1", SourceName: "decision.md", Content: "an aged decision", Score: 0.9,
				Confidence: 0.9, ValidationStatus: "verified"},
		},
		verdict: &memory.RoutingVerdict{
			Verdict:     memory.VerdictMedium,
			Guidance:    "re-confirm it is still current",
			WidenRounds: 0,
			Basis:       memory.VerdictBasis{ResultCount: 1, TrustMean: 0.84, AgeCapped: true, WeakestDim: memory.WeakestAgedDecision},
		},
	}
	te := graphTestExecutor(t, mem, nil)
	res := te.memorySearch(context.Background(), `{"query":"planning fact"}`, "snake", nil)

	if !mem.gotRouting {
		t.Fatal("memory_search must opt into Routing when the searcher supports it")
	}
	if !strings.Contains(res.Content, "retrieval_trust_verdict=medium") {
		t.Fatalf("expected verdict banner, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "guidance: re-confirm it is still current") {
		t.Fatalf("expected guidance line, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "confidence=0.90 status=verified") {
		t.Fatalf("expected per-hit trust tokens, got:\n%s", res.Content)
	}
}

// Byte-identical when routing is NOT supported: no banner, no per-hit tokens.
func TestMemorySearch_RoutingOff_NoBannerNoTokens(t *testing.T) {
	mem := &stubMemory{results: []memory.SearchResult{
		{ChunkID: "c1", SourceName: "r.md", Content: "x", Score: 0.5,
			Confidence: 0.9, ValidationStatus: "verified"},
	}}
	te := graphTestExecutor(t, mem, nil)
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	for _, tok := range []string{"retrieval_trust_verdict", "guidance:", "confidence=", "status="} {
		if strings.Contains(res.Content, tok) {
			t.Fatalf("routing-off output must not contain %q, got:\n%s", tok, res.Content)
		}
	}
	// The legacy per-hit line is still present unchanged.
	if !strings.Contains(res.Content, "source=r.md") {
		t.Fatalf("legacy hit line regressed:\n%s", res.Content)
	}
}
