package graph

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// --- in-memory fakes (read-only slices of the persistence repos) ---

type searcherEntityRepo struct {
	byID    map[string]*persistence.KnowledgeEntity
	listErr error
	getErr  error
	simErr  error
}

func (r *searcherEntityRepo) Get(_ context.Context, id string) (*persistence.KnowledgeEntity, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.byID[id], nil
}

func (r *searcherEntityRepo) List(_ context.Context, f persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := []*persistence.KnowledgeEntity{}
	for _, e := range r.byID {
		if e.ProjectID != f.ProjectID {
			continue // mirror the SQL project scope
		}
		if len(f.Types) > 0 {
			ok := false
			for _, t := range f.Types {
				if e.Type == t {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *searcherEntityRepo) SimilarByEmbedding(_ context.Context, projectID, entityType string, _ []float32, _ int) ([]*persistence.KnowledgeEntity, error) {
	if r.simErr != nil {
		return nil, r.simErr
	}
	out := []*persistence.KnowledgeEntity{}
	for _, e := range r.byID {
		if e.ProjectID == projectID && e.Type == entityType {
			out = append(out, e)
		}
	}
	return out, nil
}

type searcherEdgeRepo struct {
	byEntity map[string][]*persistence.KnowledgeEdge
	err      error
}

func (r *searcherEdgeRepo) EdgesForEntity(_ context.Context, id string, _ int) ([]*persistence.KnowledgeEdge, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.byEntity[id], nil
}

type searcherMentionRepo struct {
	byEntity map[string][]*persistence.EntityMention
	err      error
}

func (r *searcherMentionRepo) ListByEntity(_ context.Context, id string, _ int) ([]*persistence.EntityMention, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.byEntity[id], nil
}

// fakeChunkLookup is an in-memory ChunkLookup that enforces the same
// project + repo_scope rules the SQL impl does.
type fakeChunkLookup struct {
	chunks map[string]MentionedChunk // keyed by chunk id
	err    error
}

func (l *fakeChunkLookup) LookupChunks(_ context.Context, projectID string, ids []string, repoScope string) ([]MentionedChunk, error) {
	if l.err != nil {
		return nil, l.err
	}
	out := []MentionedChunk{}
	for _, id := range ids {
		c, ok := l.chunks[id]
		if !ok || c.ProjectID != projectID {
			continue
		}
		if repoScope != "" {
			if c.RepoScope != repoScope && c.RepoScope != "*" && c.RepoScope != "" {
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func ent(id, proj, typ, name string) *persistence.KnowledgeEntity {
	return &persistence.KnowledgeEntity{ID: id, ProjectID: proj, Type: typ, CanonicalName: name, LifecycleState: "published"}
}

func edge(id, proj, from, to, pred string) *persistence.KnowledgeEdge {
	return &persistence.KnowledgeEdge{ID: id, ProjectID: proj, FromEntity: from, ToEntity: to, Predicate: pred, LifecycleState: "published"}
}

// --- FindEntities ---

func TestFindEntities_ScopedToProject(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "projA", "VENDOR", "ACME"),
		"b": ent("b", "projB", "VENDOR", "ACME"), // same name, other project
	}}
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)

	got, err := s.FindEntities(context.Background(), "projA", "ACME", []string{"VENDOR"}, 10)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected only projA entity 'a', got %+v", got)
	}
}

func TestFindEntities_EmptyGraph(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{}}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	got, err := s.FindEntities(context.Background(), "projA", "anything", nil, 10)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result on empty graph, got %d", len(got))
	}
}

func TestFindEntities_EmbeddingShortlistDedup(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "projA", "VENDOR", "ACME"),
		"c": ent("c", "projA", "VENDOR", "RollerWorld"),
	}}
	// embedder returns a vec → SimilarByEmbedding returns both VENDOR
	// entities; "a" is already in via name match so it must not dup.
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, fakeEmbedder)
	got, err := s.FindEntities(context.Background(), "projA", "ACME", []string{"VENDOR"}, 10)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	seen := map[string]int{}
	for _, e := range got {
		seen[e.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("entity %s returned %d times (dup)", id, n)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct entities (name + embedding), got %d", len(got))
	}
}

func TestFindEntities_RequiresProject(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, err := s.FindEntities(context.Background(), "", "q", nil, 5); err == nil {
		t.Fatal("expected error for empty projectID")
	}
}

func TestFindEntities_ListErrorPropagates(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{listErr: errors.New("boom")}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, err := s.FindEntities(context.Background(), "p", "q", nil, 5); err == nil {
		t.Fatal("expected list error to propagate")
	}
}

func TestFindEntities_EmbeddingErrorPropagates(t *testing.T) {
	er := &searcherEntityRepo{
		byID:   map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "RollerWorld")},
		simErr: errors.New("vec boom"),
	}
	// query has no name match (so name pass returns nothing) but the
	// embedding pass runs and errors → must propagate.
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, fakeEmbedder)
	if _, err := s.FindEntities(context.Background(), "p", "ACME", []string{"VENDOR"}, 5); err == nil {
		t.Fatal("expected embedding error to propagate")
	}
}

// erroringEmbedder returns an error so FindEntities' `embErr == nil`
// guard is false and the embedding pass is silently skipped.
func erroringEmbedder(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("embed down")
}

func TestFindEntities_EmbedderErrorSkipsPass(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "VENDOR", "ACME"),
	}}
	// Name match returns "a"; embedder errors → embedding pass skipped,
	// name result still returned (no error surfaced to caller).
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, erroringEmbedder)
	got, err := s.FindEntities(context.Background(), "p", "ACME", []string{"VENDOR"}, 10)
	if err != nil {
		t.Fatalf("embedder error must not surface, got %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected name match preserved, got %+v", got)
	}
}

func TestFindEntities_SkipsNilRows(t *testing.T) {
	// List returns a nil entry the searcher must defensively skip.
	er := &nilRowEntityRepo{}
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	got, err := s.FindEntities(context.Background(), "p", "x", nil, 10)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 1 || got[0].ID != "real" {
		t.Fatalf("expected nil row skipped, got %+v", got)
	}
}

// nilRowEntityRepo.List returns a slice containing a nil entry.
type nilRowEntityRepo struct{ searcherEntityRepo }

func (r *nilRowEntityRepo) List(context.Context, persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	return []*persistence.KnowledgeEntity{nil, ent("real", "p", "X", "Real")}, nil
}

// simNilForeignRepo.SimilarByEmbedding returns a nil row + a
// foreign-project row, both of which the embedding loop must skip.
type simNilForeignRepo struct{ searcherEntityRepo }

func (r *simNilForeignRepo) List(context.Context, persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	return nil, nil // force the embedding pass to do the work
}
func (r *simNilForeignRepo) SimilarByEmbedding(context.Context, string, string, []float32, int) ([]*persistence.KnowledgeEntity, error) {
	return []*persistence.KnowledgeEntity{nil, ent("foreign", "OTHER", "VENDOR", "X"), ent("ours", "p", "VENDOR", "Ours")}, nil
}

func TestFindEntities_EmbeddingSkipsNilAndForeign(t *testing.T) {
	s := NewSearcher(&simNilForeignRepo{}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, fakeEmbedder)
	got, err := s.FindEntities(context.Background(), "p", "q", []string{"VENDOR"}, 10)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ours" {
		t.Fatalf("embedding loop should skip nil + foreign, got %+v", got)
	}
}

func TestFindEntities_EmbeddingLoopHitsLimit(t *testing.T) {
	// Name match returns nothing (query won't substring), embedding
	// pass returns 3 VENDORs; limit 1 → loop must stop after 1.
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "VENDOR", "Alpha"),
		"b": ent("b", "p", "VENDOR", "Beta"),
		"c": ent("c", "p", "VENDOR", "Gamma"),
	}}
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, fakeEmbedder)
	got, err := s.FindEntities(context.Background(), "p", "zzz-no-match", []string{"VENDOR"}, 1)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected embedding loop to stop at limit 1, got %d", len(got))
	}
}

func TestFindEntities_LimitCapAndNameHitStops(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "VENDOR", "AAAA"),
		"b": ent("b", "p", "VENDOR", "BBBB"),
		"c": ent("c", "p", "VENDOR", "CCCC"),
	}}
	// limit 0 → clamps to default; limit 2 → stops after 2 name hits
	// before the embedding pass.
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, fakeEmbedder)
	got, err := s.FindEntities(context.Background(), "p", "", []string{"VENDOR"}, 2)
	if err != nil {
		t.Fatalf("FindEntities: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected limit to cap at 2, got %d", len(got))
	}
}

// --- GetEntity ---

func TestGetEntity_SplitsEdgesByDirection(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "ACME")}}
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {
			edge("e1", "p", "a", "b", "SUPPLIES"), // outgoing
			edge("e2", "p", "c", "a", "OWNS"),     // incoming
		},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)

	got, out, in, err := s.GetEntity(context.Background(), "p", "a")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got == nil || got.ID != "a" {
		t.Fatalf("expected entity a, got %+v", got)
	}
	if len(out) != 1 || out[0].ID != "e1" {
		t.Fatalf("expected outgoing e1, got %+v", out)
	}
	if len(in) != 1 || in[0].ID != "e2" {
		t.Fatalf("expected incoming e2, got %+v", in)
	}
}

func TestGetEntity_CrossProjectReturnsNil(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "projB", "VENDOR", "ACME")}}
	// Even though edges exist for "a", asking as projA must yield nil
	// entity AND no edges — no cross-project leak.
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {edge("e1", "projB", "a", "b", "SUPPLIES")},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)

	got, out, in, err := s.GetEntity(context.Background(), "projA", "a")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got != nil || out != nil || in != nil {
		t.Fatalf("cross-project read leaked: ent=%v out=%v in=%v", got, out, in)
	}
}

func TestGetEntity_DropsForeignEdgeRows(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "ACME")}}
	// EdgesForEntity (id-only) could in principle return an edge row
	// belonging to another project; the searcher must drop it.
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {
			edge("e1", "p", "a", "b", "SUPPLIES"),
			edge("e2", "OTHER", "a", "z", "LEAK"),
		},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)
	_, out, _, err := s.GetEntity(context.Background(), "p", "a")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if len(out) != 1 || out[0].ID != "e1" {
		t.Fatalf("foreign edge leaked into result: %+v", out)
	}
}

func TestGetEntity_GetError(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{getErr: errors.New("boom")}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, _, _, err := s.GetEntity(context.Background(), "p", "a"); err == nil {
		t.Fatal("expected get error to propagate")
	}
}

func TestGetEntity_EdgesError(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "X", "A")}}
	edges := &searcherEdgeRepo{err: errors.New("edge boom")}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)
	if _, _, _, err := s.GetEntity(context.Background(), "p", "a"); err == nil {
		t.Fatal("expected edges error to propagate")
	}
}

func TestGetEntity_NotFound(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{}}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	got, _, _, err := s.GetEntity(context.Background(), "p", "missing")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for missing entity")
	}
}

func TestGetEntity_RequiresArgs(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, _, _, err := s.GetEntity(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error for empty projectID")
	}
	if _, _, _, err := s.GetEntity(context.Background(), "p", ""); err == nil {
		t.Fatal("expected error for empty entityID")
	}
}

// --- ChunksMentioning ---

func TestChunksMentioning_ScopeIsolation(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "projA", "VENDOR", "ACME")}}
	mentions := &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{
		"a": {
			{ChunkID: "c1", EntityID: "a", Surface: "ACME"},
			{ChunkID: "c2", EntityID: "a"}, // foreign chunk
		},
	}}
	lookup := &fakeChunkLookup{chunks: map[string]MentionedChunk{
		"c1": {ChunkID: "c1", ProjectID: "projA", Content: "ours"},
		"c2": {ChunkID: "c2", ProjectID: "projB", Content: "theirs"}, // must be dropped
	}}
	s := NewSearcher(er, &searcherEdgeRepo{}, mentions, lookup, nil)

	got, err := s.ChunksMentioning(context.Background(), "projA", "a", "", 10)
	if err != nil {
		t.Fatalf("ChunksMentioning: %v", err)
	}
	if len(got) != 1 || got[0].ChunkID != "c1" {
		t.Fatalf("expected only own chunk c1, got %+v", got)
	}
	if got[0].Surface != "ACME" {
		t.Fatalf("expected surface text carried through, got %q", got[0].Surface)
	}
}

func TestChunksMentioning_RepoScopeFilter(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "ACME")}}
	mentions := &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{
		"a": {{ChunkID: "c1", EntityID: "a"}, {ChunkID: "c2", EntityID: "a"}, {ChunkID: "c3", EntityID: "a"}},
	}}
	lookup := &fakeChunkLookup{chunks: map[string]MentionedChunk{
		"c1": {ChunkID: "c1", ProjectID: "p", RepoScope: "repoX"},
		"c2": {ChunkID: "c2", ProjectID: "p", RepoScope: "repoY"}, // wrong scope → dropped
		"c3": {ChunkID: "c3", ProjectID: "p", RepoScope: "*"},     // cross-cutting → kept
	}}
	s := NewSearcher(er, &searcherEdgeRepo{}, mentions, lookup, nil)

	got, err := s.ChunksMentioning(context.Background(), "p", "a", "repoX", 10)
	if err != nil {
		t.Fatalf("ChunksMentioning: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ChunkID] = true
	}
	if !ids["c1"] || !ids["c3"] || ids["c2"] {
		t.Fatalf("repo_scope filter wrong: %+v", ids)
	}
}

func TestChunksMentioning_CrossProjectEntityYieldsNothing(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "projB", "VENDOR", "ACME")}}
	mentions := &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{
		"a": {{ChunkID: "c1", EntityID: "a"}},
	}}
	lookup := &fakeChunkLookup{chunks: map[string]MentionedChunk{"c1": {ChunkID: "c1", ProjectID: "projB"}}}
	s := NewSearcher(er, &searcherEdgeRepo{}, mentions, lookup, nil)

	got, err := s.ChunksMentioning(context.Background(), "projA", "a", "", 10)
	if err != nil {
		t.Fatalf("ChunksMentioning: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-project entity leaked chunks: %+v", got)
	}
}

func TestChunksMentioning_ErrorPaths(t *testing.T) {
	ctx := context.Background()
	// required-arg guard
	s0 := NewSearcher(&searcherEntityRepo{}, &searcherEdgeRepo{}, &searcherMentionRepo{}, &fakeChunkLookup{}, nil)
	if _, err := s0.ChunksMentioning(ctx, "", "a", "", 10); err == nil {
		t.Fatal("expected error for empty projectID")
	}
	// entity Get error
	s1 := NewSearcher(&searcherEntityRepo{getErr: errors.New("boom")}, &searcherEdgeRepo{}, &searcherMentionRepo{}, &fakeChunkLookup{}, nil)
	if _, err := s1.ChunksMentioning(ctx, "p", "a", "", 10); err == nil {
		t.Fatal("expected entity get error to propagate")
	}
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "X", "A")}}
	// mentions error
	s2 := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{err: errors.New("m boom")}, &fakeChunkLookup{}, nil)
	if _, err := s2.ChunksMentioning(ctx, "p", "a", "", 10); err == nil {
		t.Fatal("expected mentions error to propagate")
	}
	// lookup error (with at least one mention so we reach the lookup)
	mr := &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{"a": {{ChunkID: "c1", EntityID: "a"}}}}
	s3 := NewSearcher(er, &searcherEdgeRepo{}, mr, &fakeChunkLookup{err: errors.New("l boom")}, nil)
	if _, err := s3.ChunksMentioning(ctx, "p", "a", "", 0); err == nil {
		t.Fatal("expected lookup error to propagate")
	}
}

func TestSubgraph_ErrorPaths(t *testing.T) {
	ctx := context.Background()
	// seed Get error
	s1 := NewSearcher(&searcherEntityRepo{getErr: errors.New("boom")}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, err := s1.Subgraph(ctx, "p", []string{"a"}, 1); err == nil {
		t.Fatal("expected seed get error to propagate")
	}
	// edges error during expansion
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "X", "A")}}
	s2 := NewSearcher(er, &searcherEdgeRepo{err: errors.New("e boom")}, &searcherMentionRepo{}, nil, nil)
	if _, err := s2.Subgraph(ctx, "p", []string{"a", "a", ""}, 1); err == nil {
		t.Fatal("expected edges error to propagate")
	}
}

func TestChunksMentioning_EmptyAndNilLookup(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "ACME")}}
	// nil lookup → error
	s := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, err := s.ChunksMentioning(context.Background(), "p", "a", "", 10); err == nil {
		t.Fatal("expected error when chunk lookup not wired")
	}
	// no mentions → empty, no error
	s2 := NewSearcher(er, &searcherEdgeRepo{}, &searcherMentionRepo{byEntity: map[string][]*persistence.EntityMention{}}, &fakeChunkLookup{}, nil)
	got, err := s2.ChunksMentioning(context.Background(), "p", "a", "", 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("expected empty no-error, got %v %+v", err, got)
	}
}

// --- Subgraph ---

func TestSubgraph_OneHop(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "VENDOR", "ACME"),
		"b": ent("b", "p", "PRODUCT", "Blinds"),
		"c": ent("c", "p", "DECISION", "chose"),
	}}
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {edge("e1", "p", "a", "b", "SUPPLIES"), edge("e2", "p", "c", "a", "PICKED")},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)

	sg, err := s.Subgraph(context.Background(), "p", []string{"a"}, 1)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if len(sg.Entities) != 3 {
		t.Fatalf("expected 3 entities (a,b,c), got %d", len(sg.Entities))
	}
	if len(sg.Edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(sg.Edges))
	}
	if sg.Hops != 1 {
		t.Fatalf("expected hops=1, got %d", sg.Hops)
	}
}

func TestSubgraph_ClampsHops(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{"a": ent("a", "p", "VENDOR", "ACME")}}
	s := NewSearcher(er, &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{}}, &searcherMentionRepo{}, nil, nil)
	sg, err := s.Subgraph(context.Background(), "p", []string{"a"}, 99)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if sg.Hops != maxSubgraphHops {
		t.Fatalf("expected hops clamped to %d, got %d", maxSubgraphHops, sg.Hops)
	}
}

func TestSubgraph_DropsForeignSeedAndEdges(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "VENDOR", "ACME"),
		"x": ent("x", "OTHER", "VENDOR", "Foreign"),
	}}
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {
			edge("e1", "p", "a", "b", "SUPPLIES"),
			edge("eLeak", "OTHER", "a", "x", "LEAK"), // foreign edge
		},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)

	// foreign seed "x" yields nothing; own seed "a" expands but the
	// foreign edge is dropped.
	sg, err := s.Subgraph(context.Background(), "p", []string{"a", "x"}, 1)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	for _, e := range sg.Edges {
		if e.ProjectID != "p" {
			t.Fatalf("foreign edge leaked: %+v", e)
		}
	}
	for _, ent := range sg.Entities {
		if ent.ProjectID != "p" {
			t.Fatalf("foreign entity leaked: %+v", ent)
		}
	}
}

func TestSubgraph_CycleTerminates(t *testing.T) {
	er := &searcherEntityRepo{byID: map[string]*persistence.KnowledgeEntity{
		"a": ent("a", "p", "X", "A"),
		"b": ent("b", "p", "X", "B"),
	}}
	// a→b and b→a form a cycle; the `loaded` set must prevent infinite
	// expansion.
	edges := &searcherEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{
		"a": {edge("e1", "p", "a", "b", "REL")},
		"b": {edge("e1", "p", "a", "b", "REL")},
	}}
	s := NewSearcher(er, edges, &searcherMentionRepo{}, nil, nil)
	sg, err := s.Subgraph(context.Background(), "p", []string{"a"}, 3)
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if len(sg.Entities) != 2 {
		t.Fatalf("expected 2 entities in cycle, got %d", len(sg.Entities))
	}
}

func TestSubgraph_RequiresProject(t *testing.T) {
	s := NewSearcher(&searcherEntityRepo{}, &searcherEdgeRepo{}, &searcherMentionRepo{}, nil, nil)
	if _, err := s.Subgraph(context.Background(), "", []string{"a"}, 1); err == nil {
		t.Fatal("expected error for empty projectID")
	}
}
