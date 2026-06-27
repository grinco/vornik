package memory

import (
	"context"
	"errors"
	"sort"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// ---- fakes ----

type fakeEntityRepo struct {
	byName  map[string][]*persistence.KnowledgeEntity
	byID    map[string]*persistence.KnowledgeEntity
	listErr error
	getErr  error
}

func (f *fakeEntityRepo) Insert(context.Context, *persistence.KnowledgeEntity) error { return nil }
func (f *fakeEntityRepo) Get(_ context.Context, id string) (*persistence.KnowledgeEntity, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byID[id], nil
}
func (f *fakeEntityRepo) GetByCanonical(context.Context, string, string, string) (*persistence.KnowledgeEntity, error) {
	return nil, nil
}
func (f *fakeEntityRepo) List(_ context.Context, filter persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byName[filter.NameLike], nil
}
func (f *fakeEntityRepo) SimilarByEmbedding(context.Context, string, string, []float32, int) ([]*persistence.KnowledgeEntity, error) {
	return nil, nil
}
func (f *fakeEntityRepo) UpdateLifecycle(context.Context, string, string) error { return nil }
func (f *fakeEntityRepo) AddAlias(context.Context, string, string) error        { return nil }

type fakeEdgeRepo struct {
	byEntity map[string][]*persistence.KnowledgeEdge
	err      error
}

func (f *fakeEdgeRepo) UpsertEdge(context.Context, *persistence.KnowledgeEdge) error { return nil }
func (f *fakeEdgeRepo) Get(context.Context, string) (*persistence.KnowledgeEdge, error) {
	return nil, nil
}
func (f *fakeEdgeRepo) List(context.Context, persistence.KnowledgeEdgeFilter) ([]*persistence.KnowledgeEdge, error) {
	return nil, nil
}
func (f *fakeEdgeRepo) EdgesForEntity(_ context.Context, id string, _ int) ([]*persistence.KnowledgeEdge, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byEntity[id], nil
}
func (f *fakeEdgeRepo) UpdateLifecycle(context.Context, string, string) error     { return nil }
func (f *fakeEdgeRepo) DropChunkFromSources(context.Context, string) (int, error) { return 0, nil }

// ---- tests ----

func TestTokenizeQueryForExpansion(t *testing.T) {
	cases := map[string][]string{
		"":                  nil,
		"a":                 nil, // too short
		"alpha":             {"alpha"},
		"AlphaBeta Gamma":   {"alphabeta", "gamma"},
		"hyphen-ated words": {"hyphen", "ated", "words"}, // length-3 floor splits hyphenated
		"123 abc":           {"123", "abc"},
		"!@#$%":             nil,
	}
	for in, want := range cases {
		got := tokenizeQueryForExpansion(in)
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: got %v, want %v", in, got, want)
				break
			}
		}
	}
}

func TestMergeExpansionIntoQuery(t *testing.T) {
	if got := mergeExpansionIntoQuery("hello", nil); got != "hello" {
		t.Fatalf("empty expansion: %q", got)
	}
	// Plain append.
	if got := mergeExpansionIntoQuery("deploy script", []string{"kubernetes", "Postgres"}); got != "deploy script kubernetes postgres" {
		t.Fatalf("plain: %q", got)
	}
	// Dedup against original tokens (case-insensitive).
	if got := mergeExpansionIntoQuery("Postgres backups", []string{"postgres", "wal"}); got != "Postgres backups wal" {
		t.Fatalf("dedup: %q", got)
	}
	// Skip empty/whitespace terms.
	if got := mergeExpansionIntoQuery("q", []string{"", "  ", "alpha"}); got != "q alpha" {
		t.Fatalf("empty terms: %q", got)
	}
}

func TestDecodeAliases(t *testing.T) {
	if got := decodeAliases(nil); got != nil {
		t.Fatalf("nil: %v", got)
	}
	if got := decodeAliases([]byte(`["alpha","beta"]`)); len(got) != 2 || got[0] != "alpha" {
		t.Fatalf("ok: %v", got)
	}
	if got := decodeAliases([]byte(`{not array}`)); got != nil {
		t.Fatalf("bad json: %v", got)
	}
}

func TestKGQueryExpander_NilGuards(t *testing.T) {
	var nilE *KGQueryExpander
	if got := nilE.Expand(context.Background(), "p", "q"); got != nil {
		t.Fatal("nil receiver")
	}
	// Missing repos.
	e := &KGQueryExpander{}
	if got := e.Expand(context.Background(), "p", "q"); got != nil {
		t.Fatal("missing repos")
	}
	// Empty project ID.
	e = &KGQueryExpander{Entities: &fakeEntityRepo{}, Edges: &fakeEdgeRepo{}}
	if got := e.Expand(context.Background(), "", "q"); got != nil {
		t.Fatal("empty project")
	}
}

func TestKGQueryExpander_NoSeedsReturnsNil(t *testing.T) {
	er := &fakeEntityRepo{byName: map[string][]*persistence.KnowledgeEntity{}}
	e := &KGQueryExpander{Entities: er, Edges: &fakeEdgeRepo{}}
	if got := e.Expand(context.Background(), "p", "alpha beta"); got != nil {
		t.Fatalf("no seeds: %v", got)
	}
}

func TestKGQueryExpander_AliasesAndNeighbors(t *testing.T) {
	seed := &persistence.KnowledgeEntity{
		ID: "ent-1", ProjectID: "p", CanonicalName: "Postgres",
		Aliases: []byte(`["postgresql","pgsql"]`),
	}
	neighbor := &persistence.KnowledgeEntity{
		ID: "ent-2", ProjectID: "p", CanonicalName: "WAL Archive",
	}
	er := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{"postgres": {seed}},
		byID:   map[string]*persistence.KnowledgeEntity{"ent-1": seed, "ent-2": neighbor},
	}
	edges := []*persistence.KnowledgeEdge{
		{ID: "e1", FromEntity: "ent-1", ToEntity: "ent-2"},
	}
	edr := &fakeEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{"ent-1": edges}}

	e := &KGQueryExpander{Entities: er, Edges: edr}
	got := e.Expand(context.Background(), "p", "postgres backups")
	sort.Strings(got)
	want := []string{"pgsql", "postgresql", "wal archive"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestKGQueryExpander_EdgesIncomingTreatedSymmetric(t *testing.T) {
	seed := &persistence.KnowledgeEntity{ID: "ent-1", ProjectID: "p", CanonicalName: "Foo"}
	other := &persistence.KnowledgeEntity{ID: "ent-2", ProjectID: "p", CanonicalName: "Bar"}
	er := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{"foo": {seed}},
		byID:   map[string]*persistence.KnowledgeEntity{"ent-1": seed, "ent-2": other},
	}
	// Edge points INTO the seed (ToEntity=seed.ID).
	edges := []*persistence.KnowledgeEdge{
		{ID: "e1", FromEntity: "ent-2", ToEntity: "ent-1"},
	}
	edr := &fakeEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{"ent-1": edges}}

	e := &KGQueryExpander{Entities: er, Edges: edr}
	got := e.Expand(context.Background(), "p", "foo")
	if len(got) != 1 || got[0] != "bar" {
		t.Fatalf("incoming edge not handled: %v", got)
	}
}

func TestKGQueryExpander_DBErrorsToleratedSilently(t *testing.T) {
	er := &fakeEntityRepo{listErr: errors.New("list down")}
	e := &KGQueryExpander{Entities: er, Edges: &fakeEdgeRepo{}}
	if got := e.Expand(context.Background(), "p", "postgres"); got != nil {
		t.Fatalf("list err should yield nil: %v", got)
	}

	er2 := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{
			"postgres": {{ID: "ent-1", CanonicalName: "Postgres"}},
		},
	}
	edr2 := &fakeEdgeRepo{err: errors.New("edge down")}
	e = &KGQueryExpander{Entities: er2, Edges: edr2}
	// Edges error → seed aliases still returned (none here), no panic.
	if got := e.Expand(context.Background(), "p", "postgres"); len(got) != 0 {
		t.Fatalf("edge err: %v", got)
	}
}

func TestKGQueryExpander_EmptyQueryReturnsNil(t *testing.T) {
	e := &KGQueryExpander{Entities: &fakeEntityRepo{}, Edges: &fakeEdgeRepo{}}
	if got := e.Expand(context.Background(), "p", ""); got != nil {
		t.Fatalf("empty query: %v", got)
	}
	if got := e.Expand(context.Background(), "p", "!!!"); got != nil {
		t.Fatalf("punctuation-only query: %v", got)
	}
}

func TestKGQueryExpander_NeighborGetErrorTolerated(t *testing.T) {
	seed := &persistence.KnowledgeEntity{ID: "ent-1", ProjectID: "p", CanonicalName: "Seed"}
	er := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{"seed": {seed}},
		// byID is empty so Get for the neighbor returns nil, nil
		// (treated as "skip this neighbor" in the expander).
		byID: map[string]*persistence.KnowledgeEntity{},
	}
	edges := []*persistence.KnowledgeEdge{
		{ID: "e1", FromEntity: "ent-1", ToEntity: "missing-id"},
	}
	edr := &fakeEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{"ent-1": edges}}
	e := &KGQueryExpander{Entities: er, Edges: edr}
	// Should not panic; should not add the missing neighbour.
	got := e.Expand(context.Background(), "p", "seed")
	if len(got) != 0 {
		t.Fatalf("missing neighbour leaked: %v", got)
	}
}

func TestKGQueryExpander_SkipsEdgeWithBlankOther(t *testing.T) {
	seed := &persistence.KnowledgeEntity{ID: "ent-1", ProjectID: "p", CanonicalName: "Seed"}
	er := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{"seed": {seed}},
		byID:   map[string]*persistence.KnowledgeEntity{},
	}
	// Both endpoints empty (corrupt edge) — must be skipped without panic.
	edges := []*persistence.KnowledgeEdge{
		{ID: "e1", FromEntity: "", ToEntity: ""},
		nil,
	}
	edr := &fakeEdgeRepo{byEntity: map[string][]*persistence.KnowledgeEdge{"ent-1": edges}}
	e := &KGQueryExpander{Entities: er, Edges: edr}
	if got := e.Expand(context.Background(), "p", "seed"); len(got) != 0 {
		t.Fatalf("corrupt edges leaked: %v", got)
	}
}

func TestKGQueryExpander_MaxSeedsAndNeighborsApplied(t *testing.T) {
	// Three tokens, three entities — MaxSeeds=1 should stop after first.
	er := &fakeEntityRepo{
		byName: map[string][]*persistence.KnowledgeEntity{
			"alpha": {{ID: "a", CanonicalName: "Alpha"}},
			"beta":  {{ID: "b", CanonicalName: "Beta"}},
			"gamma": {{ID: "c", CanonicalName: "Gamma"}},
		},
		byID: map[string]*persistence.KnowledgeEntity{
			"a": {ID: "a", CanonicalName: "Alpha"},
			"b": {ID: "b", CanonicalName: "Beta"},
			"c": {ID: "c", CanonicalName: "Gamma"},
		},
	}
	edges := map[string][]*persistence.KnowledgeEdge{
		"a": {
			{ID: "e1", FromEntity: "a", ToEntity: "b"},
			{ID: "e2", FromEntity: "a", ToEntity: "c"},
		},
	}
	edr := &fakeEdgeRepo{byEntity: edges}
	e := &KGQueryExpander{Entities: er, Edges: edr, MaxSeeds: 1, MaxNeighbors: 1}
	got := e.Expand(context.Background(), "p", "alpha beta gamma")
	// Only the first seed ("alpha") + 1 neighbour expanded.
	if len(got) != 1 {
		t.Fatalf("expected 1 expansion term, got %v", got)
	}
}
