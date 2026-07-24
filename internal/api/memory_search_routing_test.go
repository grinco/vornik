package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Confidence-based retrieval routing (P3) — REST surface tests.

// routingStub implements BOTH MemorySearcher and MemoryRoutingSearcher so the
// handler takes the routing path.
type routingStub struct {
	results []MemorySearchResult
	verdict *RoutingVerdictWire
}

func (r *routingStub) Search(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
	return r.results, nil
}

func (r *routingStub) SearchRouting(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, *RoutingVerdictWire, error) {
	return r.results, r.verdict, nil
}

func TestMemorySearch_RoutingOn_EmitsVerdictAndTrustFields(t *testing.T) {
	stub := &routingStub{
		results: []MemorySearchResult{
			{ChunkID: "c1", Content: "hit-1", Confidence: 0.9, ValidationStatus: "verified"},
		},
		verdict: &RoutingVerdictWire{
			Verdict:     "medium",
			Guidance:    "verify key facts",
			WidenRounds: 1,
			Basis:       RoutingVerdictBasisWire{ResultCount: 1, TrustMean: 0.61, WeakestDim: "trust_mean"},
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"retrieval_trust_verdict":"medium"`) {
		t.Fatalf("want verdict in body, got %s", body)
	}
	if !strings.Contains(body, `"guidance":"verify key facts"`) {
		t.Fatalf("want guidance in body, got %s", body)
	}
	if !strings.Contains(body, `"weakest_dim":"trust_mean"`) {
		t.Fatalf("want basis in body, got %s", body)
	}
	var resp memorySearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Results[0].Confidence != 0.9 || resp.Results[0].ValidationStatus != "verified" {
		t.Fatalf("want per-result trust fields, got %+v", resp.Results[0])
	}
}

// Byte-identical when routing is NOT supported (plain searcher): the response
// must carry no routing keys at all — the hard back-compat contract.
func TestMemorySearch_RoutingOff_ByteIdenticalShape(t *testing.T) {
	stub := &stubMemorySearcher{
		searchFn: func(_ context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
			return []MemorySearchResult{{ChunkID: "c1", Content: "hit-1"}}, nil
		},
	}
	srv := NewServer(WithMemorySearcher(stub))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=hi", nil)
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, k := range []string{"retrieval_trust_verdict", "guidance", "verdict_basis", "widen_rounds", "confidence", "validation_status"} {
		if strings.Contains(body, k) {
			t.Fatalf("routing-off response must not contain %q; body=%s", k, body)
		}
	}
	// The exact legacy shape: a single results array with the pre-feature
	// MemorySearchResult fields (the non-omitempty ones always present). The
	// new routing keys + trust fields must be entirely absent.
	want := `{"results":[{"chunk_id":"c1","project_id":"","task_id":"","source_name":"","content":"hit-1","score":0}]}` + "\n"
	if body != want {
		t.Fatalf("routing-off body not byte-identical to legacy shape:\n got: %q\nwant: %q", body, want)
	}
}
