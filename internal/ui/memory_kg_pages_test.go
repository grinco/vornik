package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
)

// fakeKGReader is an in-memory KnowledgeGraphReader for the UI page
// tests. It records the projectID every call was scoped to so the
// tests can assert no cross-project pivot.
type fakeKGReader struct {
	entities []*persistence.KnowledgeEntity
	byID     map[string]*persistence.KnowledgeEntity
	outgoing map[string][]*persistence.KnowledgeEdge
	incoming map[string][]*persistence.KnowledgeEdge
	chunks   map[string][]graph.MentionedChunk
	subgraph *graph.Subgraph
	lastProj string
	findErr  error
	getErr   error
	subErr   error
	chunkErr error
}

func (f *fakeKGReader) FindEntities(_ context.Context, projectID, _ string, _ []string, _ int) ([]*persistence.KnowledgeEntity, error) {
	f.lastProj = projectID
	if f.findErr != nil {
		return nil, f.findErr
	}
	out := []*persistence.KnowledgeEntity{}
	for _, e := range f.entities {
		if e == nil {
			out = append(out, nil) // exercise the handler's nil-skip guard
			continue
		}
		if e.ProjectID == projectID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeKGReader) GetEntity(_ context.Context, projectID, id string) (*persistence.KnowledgeEntity, []*persistence.KnowledgeEdge, []*persistence.KnowledgeEdge, error) {
	f.lastProj = projectID
	if f.getErr != nil {
		return nil, nil, nil, f.getErr
	}
	e := f.byID[id]
	if e == nil || e.ProjectID != projectID {
		return nil, nil, nil, nil // not found / cross-project
	}
	return e, f.outgoing[id], f.incoming[id], nil
}

func (f *fakeKGReader) ChunksMentioning(_ context.Context, projectID, id, _ string, _ int) ([]graph.MentionedChunk, error) {
	f.lastProj = projectID
	if f.chunkErr != nil {
		return nil, f.chunkErr
	}
	return f.chunks[id], nil
}

func (f *fakeKGReader) Subgraph(_ context.Context, projectID string, _ []string, _ int) (*graph.Subgraph, error) {
	f.lastProj = projectID
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.subgraph, nil
}

func kgEnt(id, proj, typ, name string) *persistence.KnowledgeEntity {
	return &persistence.KnowledgeEntity{ID: id, ProjectID: proj, Type: typ, CanonicalName: name}
}

func TestMemoryEntities_RendersGroupedByType(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{entities: []*persistence.KnowledgeEntity{
		kgEnt("e1", "p1", "VENDOR", "ACME Blinds"),
		kgEnt("e2", "p1", "DECISION", "chose Velux"),
		kgEnt("eX", "other", "VENDOR", "Foreign Co"), // must not appear
		nil, // handler must skip nil rows defensively
	}}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "ACME Blinds")
	assert.Contains(t, body, "chose Velux")
	assert.Contains(t, body, "VENDOR")
	assert.NotContains(t, body, "Foreign Co", "cross-project entity must not render")
	assert.Equal(t, "p1", srv.kgSearcher.(*fakeKGReader).lastProj)
}

func TestMemoryEntities_NotEnabled(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	// no kgSearcher wired
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "not enabled")
}

func TestMemoryEntityDetail_RendersEdgesAndChunks(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{
		byID: map[string]*persistence.KnowledgeEntity{
			"e1": {ID: "e1", ProjectID: "p1", Type: "VENDOR", CanonicalName: "ACME Blinds", Description: "supplier", Aliases: []byte(`["ACME","ACME Inc"]`)},
			"e2": {ID: "e2", ProjectID: "p1", Type: "PRODUCT", CanonicalName: "Roman blinds"},
		},
		outgoing: map[string][]*persistence.KnowledgeEdge{
			"e1": {{ID: "ed1", ProjectID: "p1", FromEntity: "e1", ToEntity: "e2", Predicate: "SUPPLIES", SourceChunks: []string{"c1"}}},
		},
		chunks: map[string][]graph.MentionedChunk{
			"e1": {{ChunkID: "c1", ProjectID: "p1", SourceName: "research.md", Content: "ACME quote", Surface: "ACME"}},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "ACME Blinds")
	assert.Contains(t, body, "supplier")
	assert.Contains(t, body, "ACME Inc")     // alias
	assert.Contains(t, body, "SUPPLIES")     // edge predicate
	assert.Contains(t, body, "Roman blinds") // resolved neighbour name
	assert.Contains(t, body, "research.md")  // mentioning chunk
	assert.Contains(t, body, "ACME quote")
}

func TestMemoryEntityDetail_CrossProjectIs404Body(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{byID: map[string]*persistence.KnowledgeEntity{
		"eX": {ID: "eX", ProjectID: "other", Type: "VENDOR", CanonicalName: "Foreign Co"},
	}}
	// Ask as p1 for an entity owned by "other" → must show not-found,
	// never the foreign entity's name.
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities/eX", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "not found")
	assert.NotContains(t, body, "Foreign Co", "cross-project entity leaked into detail page")
}

func TestMemorySubgraph_RendersSVG(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{subgraph: &graph.Subgraph{
		ProjectID: "p1",
		Hops:      1,
		Entities: []*persistence.KnowledgeEntity{
			{ID: "e1", ProjectID: "p1", Type: "VENDOR", CanonicalName: "ACME"},
			{ID: "e2", ProjectID: "p1", Type: "PRODUCT", CanonicalName: "Blinds"},
		},
		Edges: []*persistence.KnowledgeEdge{
			{ID: "ed1", ProjectID: "p1", FromEntity: "e1", ToEntity: "e2", Predicate: "DEPENDS_ON"},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/subgraph/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "<svg")
	assert.Contains(t, body, "ACME")
	assert.Contains(t, body, "DEPENDS_ON")
	assert.Contains(t, body, "<circle") // a node was plotted
}

func TestMemorySubgraph_EmptyState(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{subgraph: &graph.Subgraph{ProjectID: "p1", Hops: 1}}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/subgraph/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No subgraph")
}

func TestKGHelpers(t *testing.T) {
	assert.True(t, isClosedPredicate("depends_on"))
	assert.False(t, isClosedPredicate("CUSTOM_REL"))
	assert.Equal(t, "#6b7280", predicateColour("CUSTOM_REL", false))
	// Every closed-vocab colour branch.
	assert.Equal(t, "#34d399", predicateColour(persistence.PredicateDependsOn, true))
	assert.Equal(t, "#34d399", predicateColour(persistence.PredicateRelatesTo, true))
	assert.Equal(t, "#a78bfa", predicateColour(persistence.PredicateOwnedBy, true))
	assert.Equal(t, "#a78bfa", predicateColour(persistence.PredicateChosenOver, true))
	assert.Equal(t, "#fbbf24", predicateColour(persistence.PredicateHasDeadline, true))
	assert.Equal(t, "#fbbf24", predicateColour(persistence.PredicateQuotedPrice, true))
	assert.Equal(t, "#60a5fa", predicateColour(persistence.PredicateMentionedIn, true)) // other closed
	assert.Equal(t, []string{"a", "b"}, decodeJSONStringArray([]byte(`["a","b"]`)))
	assert.Nil(t, decodeJSONStringArray([]byte(`not json`)))
	assert.Nil(t, decodeJSONStringArray([]byte(`[bad json]`)))
	assert.Nil(t, decodeJSONStringArray(nil))
	assert.Equal(t, "fallback", nameOr(map[string]string{}, "fallback"))
	assert.Equal(t, "name", nameOr(map[string]string{"id": "name"}, "id"))
}

func TestMemoryEntities_FindError_RendersEnabledEmpty(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{findErr: assert.AnError}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities?type=VENDOR&q=acme", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// Error path still renders the page (enabled) with no groups.
	assert.Contains(t, rec.Body.String(), "No entities")
}

func TestMemoryEntityDetail_GetError500(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{getErr: assert.AnError}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestMemoryEntityDetail_NotEnabled(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "not enabled")
}

func TestMemoryEntityDetail_ChunksErrorStillRenders(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{
		byID: map[string]*persistence.KnowledgeEntity{
			"e1": {ID: "e1", ProjectID: "p1", Type: "VENDOR", CanonicalName: "ACME Blinds"},
		},
		incoming: map[string][]*persistence.KnowledgeEdge{
			// incoming edge whose neighbour resolveName falls back to id
			"e1": {{ID: "ed9", ProjectID: "p1", FromEntity: "unknownX", ToEntity: "e1", Predicate: "RELATES_TO"}},
		},
		chunkErr: assert.AnError, // chunk fetch fails → logged, page still renders
	}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/entities/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "ACME Blinds")
	assert.Contains(t, body, "RELATES_TO")
	assert.Contains(t, body, "unknownX") // unresolved neighbour falls back to id
}

func TestMemorySubgraph_Error500(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv.kgSearcher = &fakeKGReader{subErr: assert.AnError}
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/subgraph/e1?hops=2", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestMemorySubgraph_NotEnabled(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	req := httptest.NewRequest(http.MethodGet, "/memory/p1/subgraph/e1", nil)
	rec := httptest.NewRecorder()
	srv.memoryRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "not enabled")
}

// Bare /subgraph (no entity id) and unknown action 404.
func TestMemoryRouter_KGEdgeCases(t *testing.T) {
	srv, _ := swarmEditServer(t, writeSwarmFixture(t))
	for _, path := range []string{"/memory/p1/subgraph", "/memory/p1/subgraph/", "/memory/p1/bogus"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.memoryRouter(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path %s", path)
	}
}
