package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeGraphSearcher is a deterministic in-memory GraphSearcher for
// the dispatcher's memory_search overlay tests.
type fakeGraphSearcher struct {
	entities []*persistence.KnowledgeEntity
	outgoing map[string][]*persistence.KnowledgeEdge
	incoming map[string][]*persistence.KnowledgeEdge
	findErr  error
	getErr   error
	projSeen string // last projectID FindEntities was called with
}

func (f *fakeGraphSearcher) FindEntities(_ context.Context, projectID, _ string, _ []string, _ int) ([]*persistence.KnowledgeEntity, error) {
	f.projSeen = projectID
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.entities, nil
}

func (f *fakeGraphSearcher) GetEntity(_ context.Context, _, entityID string) (*persistence.KnowledgeEntity, []*persistence.KnowledgeEdge, []*persistence.KnowledgeEdge, error) {
	if f.getErr != nil {
		return nil, nil, nil, f.getErr
	}
	return nil, f.outgoing[entityID], f.incoming[entityID], nil
}

func withGraphSearcher(g GraphSearcher) func(*ToolExecutor) {
	return func(te *ToolExecutor) { te.graphSearcher = g }
}

func graphTestExecutor(t *testing.T, mem MemorySearcher, g GraphSearcher) *ToolExecutor {
	t.Helper()
	reg := newRegistryWith(t, []registry.Project{{ID: "snake"}}, nil, nil)
	te := &ToolExecutor{logger: zerolog.Nop()}
	withRegistry(reg)(te)
	if mem != nil {
		withMemory(mem)(te)
	}
	if g != nil {
		withGraphSearcher(g)(te)
	}
	return te
}

func TestMemorySearch_AppendsGraphBlock(t *testing.T) {
	mem := &stubMemory{results: []memory.SearchResult{
		{ChunkID: "c1", SourceName: "research.md", Content: "ACME quote", Score: 0.9},
	}}
	g := &fakeGraphSearcher{
		entities: []*persistence.KnowledgeEntity{
			{ID: "e1", ProjectID: "snake", Type: "VENDOR", CanonicalName: "ACME Blinds", Description: "supplier"},
		},
		outgoing: map[string][]*persistence.KnowledgeEdge{
			"e1": {{ID: "ed1", ProjectID: "snake", FromEntity: "e1", ToEntity: "Roman blinds", Predicate: "SUPPLIES"}},
		},
		incoming: map[string][]*persistence.KnowledgeEdge{
			"e1": {{ID: "ed2", ProjectID: "snake", FromEntity: "decision_3", ToEntity: "e1", Predicate: "PICKED"}},
		},
	}
	te := graphTestExecutor(t, mem, g)

	res := te.memorySearch(context.Background(), `{"query":"ACME"}`, "snake", nil)
	if !strings.Contains(res.Content, "Knowledge-graph entities") {
		t.Fatalf("expected KG block, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "ACME Blinds [VENDOR]") {
		t.Fatalf("expected entity line, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "→ SUPPLIES Roman blinds") {
		t.Fatalf("expected outgoing edge, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "← decision_3 PICKED") {
		t.Fatalf("expected incoming edge, got:\n%s", res.Content)
	}
	// Chunk hits must still be present (no regression).
	if !strings.Contains(res.Content, "research.md") {
		t.Fatalf("chunk hits regressed, got:\n%s", res.Content)
	}
	if g.projSeen != "snake" {
		t.Fatalf("graph searcher not scoped to project: %q", g.projSeen)
	}
}

func TestMemorySearch_NoGraphSearcher_ChunkOnly(t *testing.T) {
	mem := &stubMemory{results: []memory.SearchResult{{ChunkID: "c1", SourceName: "r.md", Content: "x"}}}
	te := graphTestExecutor(t, mem, nil)
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if strings.Contains(res.Content, "Knowledge-graph") {
		t.Fatalf("KG block must not appear without a graph searcher:\n%s", res.Content)
	}
}

func TestMemorySearch_EmptyGraph_NoBlock(t *testing.T) {
	mem := &stubMemory{results: []memory.SearchResult{{ChunkID: "c1", SourceName: "r.md", Content: "x"}}}
	g := &fakeGraphSearcher{entities: nil} // empty graph
	te := graphTestExecutor(t, mem, g)
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	if strings.Contains(res.Content, "Knowledge-graph") {
		t.Fatalf("empty graph must not add a block:\n%s", res.Content)
	}
}

func TestMemorySearch_GraphError_NonFatal(t *testing.T) {
	mem := &stubMemory{results: []memory.SearchResult{{ChunkID: "c1", SourceName: "r.md", Content: "x"}}}
	g := &fakeGraphSearcher{findErr: errors.New("boom")}
	te := graphTestExecutor(t, mem, g)
	res := te.memorySearch(context.Background(), `{"query":"x"}`, "snake", nil)
	// Chunk answer still returned; no KG block, no failure.
	if !strings.Contains(res.Content, "r.md") || strings.Contains(res.Content, "Knowledge-graph") {
		t.Fatalf("graph error should be silently skipped, got:\n%s", res.Content)
	}
}

func TestGraphContextBlock_NilSearcher(t *testing.T) {
	te := &ToolExecutor{logger: zerolog.Nop()}
	if got := te.graphContextBlock(context.Background(), "p", "q"); got != "" {
		t.Fatalf("expected empty block with nil searcher, got %q", got)
	}
}

func TestGraphContextBlock_TruncatesAndCapsEdges(t *testing.T) {
	longDesc := strings.Repeat("x", 300)
	// 8 outgoing edges → must cap at maxEdgesPerEntity (6).
	outs := make([]*persistence.KnowledgeEdge, 0, 8)
	for i := 0; i < 8; i++ {
		outs = append(outs, &persistence.KnowledgeEdge{ID: "e", ProjectID: "p", FromEntity: "e1", ToEntity: "t", Predicate: "REL"})
	}
	g := &fakeGraphSearcher{
		entities: []*persistence.KnowledgeEntity{{ID: "e1", ProjectID: "p", Type: "VENDOR", CanonicalName: "ACME", Description: longDesc}},
		outgoing: map[string][]*persistence.KnowledgeEdge{"e1": outs},
	}
	te := &ToolExecutor{logger: zerolog.Nop(), graphSearcher: g}
	block := te.graphContextBlock(context.Background(), "p", "q")
	if !strings.Contains(block, "…") {
		t.Fatalf("expected description truncation marker, got:\n%s", block)
	}
	if n := strings.Count(block, "→ REL t"); n != 6 {
		t.Fatalf("expected 6 capped edges, got %d:\n%s", n, block)
	}
}

func TestGraphContextBlock_GetEntityError_SkipsEdges(t *testing.T) {
	g := &fakeGraphSearcher{
		entities: []*persistence.KnowledgeEntity{{ID: "e1", ProjectID: "p", Type: "VENDOR", CanonicalName: "ACME"}},
		getErr:   errors.New("boom"),
	}
	te := &ToolExecutor{logger: zerolog.Nop(), graphSearcher: g}
	block := te.graphContextBlock(context.Background(), "p", "q")
	// Entity line still present; edges skipped because GetEntity errored.
	if !strings.Contains(block, "ACME [VENDOR]") {
		t.Fatalf("expected entity line, got:\n%s", block)
	}
}
