package graph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// fakeEntityRepo is an in-memory KnowledgeEntityRepository good
// enough to drive the resolver. Only the methods the resolver
// touches (SimilarByEmbedding, List) are wired; the rest panic
// so missed wiring surfaces loudly in tests.
type fakeEntityRepo struct {
	similar []*persistence.KnowledgeEntity
	listed  []*persistence.KnowledgeEntity
	gotProj string
	gotType string
}

func (f *fakeEntityRepo) Insert(context.Context, *persistence.KnowledgeEntity) error {
	panic("not used")
}
func (f *fakeEntityRepo) Get(context.Context, string) (*persistence.KnowledgeEntity, error) {
	panic("not used")
}
func (f *fakeEntityRepo) GetByCanonical(context.Context, string, string, string) (*persistence.KnowledgeEntity, error) {
	panic("not used")
}
func (f *fakeEntityRepo) List(_ context.Context, filter persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error) {
	f.gotProj = filter.ProjectID
	if len(filter.Types) > 0 {
		f.gotType = filter.Types[0]
	}
	return f.listed, nil
}
func (f *fakeEntityRepo) SimilarByEmbedding(_ context.Context, projectID, entityType string, _ []float32, _ int) ([]*persistence.KnowledgeEntity, error) {
	f.gotProj = projectID
	f.gotType = entityType
	return f.similar, nil
}
func (f *fakeEntityRepo) UpdateLifecycle(context.Context, string, string) error { panic("not used") }
func (f *fakeEntityRepo) AddAlias(context.Context, string, string) error        { panic("not used") }

func fakeEmbedder(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}

func TestResolver_ShortCircuitsOnNearMatch(t *testing.T) {
	repo := &fakeEntityRepo{
		similar: []*persistence.KnowledgeEntity{
			{ID: "kent-1", CanonicalName: "PostgreSQL 16", Type: "TECHNOLOGY", Embedding: []float32{0.1, 0.2, 0.3}},
		},
	}
	r := NewResolver(nil, "fake", repo, fakeEmbedder)

	cands := []Candidate{{Type: "TECHNOLOGY", Name: "Postgresql 16"}} // case + small whitespace diff
	res, m, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 1 || res[0].Decision != "match" || res[0].MatchID != "kent-1" {
		t.Fatalf("expected match → kent-1, got %+v", res)
	}
	if !res[0].ShortCircuit {
		t.Errorf("expected short-circuit, got %+v", res[0])
	}
	if m.ShortCircuited != 1 || m.LLMResolved != 0 {
		t.Errorf("metrics off: %+v", m)
	}
}

func TestResolver_FarMatchFallsThroughToLLM(t *testing.T) {
	repo := &fakeEntityRepo{
		similar: []*persistence.KnowledgeEntity{
			{ID: "kent-1", CanonicalName: "ACME Plumbing Inc", Type: "VENDOR"},
		},
	}
	llmReply := `[{"candidate_id":"cand-0","decision":"new","reason":"different domain"}]`
	fp := &fakeProvider{replies: []reply{{content: llmReply}}}
	r := NewResolver(fp, "fake", repo, fakeEmbedder)

	cands := []Candidate{{Type: "VENDOR", Name: "ACME Blinds"}}
	res, m, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 1 || res[0].Decision != "new" {
		t.Fatalf("expected new from LLM, got %+v", res)
	}
	if res[0].ShortCircuit {
		t.Errorf("did not expect short-circuit on far names")
	}
	if m.ShortCircuited != 0 || m.LLMResolved != 1 {
		t.Errorf("metrics off: %+v", m)
	}
	if fp.calls.Load() != 1 {
		t.Errorf("expected 1 LLM call, got %d", fp.calls.Load())
	}
}

func TestResolver_BatchedLLMDecisions(t *testing.T) {
	repo := &fakeEntityRepo{
		similar: []*persistence.KnowledgeEntity{
			{ID: "kent-A", CanonicalName: "Globex Industries"},
		},
	}
	// Two LLM-bound candidates (unrelated names so the gate can't
	// fire) → resolver issues ONE batched call. Echo IDs back in
	// reverse order to confirm we re-correlate by candidate_id.
	llmReply := `[
	  {"candidate_id":"cand-1","decision":"new","reason":"second"},
	  {"candidate_id":"cand-0","decision":"match","match_id":"kent-A","merge_aliases":["Globex"],"reason":"first"}
	]`
	fp := &fakeProvider{replies: []reply{{content: llmReply}}}
	r := NewResolver(fp, "fake", repo, fakeEmbedder)

	cands := []Candidate{
		{Type: "VENDOR", Name: "Globex"},
		{Type: "VENDOR", Name: "Initech LLC"},
	}
	res, _, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 resolutions, got %d", len(res))
	}
	if res[0].Decision != "match" || res[0].MatchID != "kent-A" {
		t.Errorf("res[0] expected match→kent-A, got %+v", res[0])
	}
	if res[1].Decision != "new" {
		t.Errorf("res[1] expected new, got %+v", res[1])
	}
	if fp.calls.Load() != 1 {
		t.Errorf("expected 1 batched LLM call, got %d", fp.calls.Load())
	}
}

func TestResolver_MissingDecisionFallsBackToAmbiguous(t *testing.T) {
	repo := &fakeEntityRepo{
		similar: []*persistence.KnowledgeEntity{
			{ID: "kent-X", CanonicalName: "Faraway Co"},
		},
	}
	llmReply := `[]` // model returned nothing
	fp := &fakeProvider{replies: []reply{{content: llmReply}}}
	r := NewResolver(fp, "fake", repo, fakeEmbedder)

	cands := []Candidate{{Type: "VENDOR", Name: "ZeroCo"}}
	res, _, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 1 || res[0].Decision != "ambiguous" {
		t.Fatalf("expected ambiguous fallback, got %+v", res)
	}
}

func TestResolver_FallsBackToListWhenEmbeddingEmpty(t *testing.T) {
	repo := &fakeEntityRepo{
		similar: nil, // embedder produced vectors but DB returned nothing
		listed:  []*persistence.KnowledgeEntity{{ID: "kent-N", CanonicalName: "ACME Blinds"}},
	}
	r := NewResolver(nil, "fake", repo, fakeEmbedder) // nil client → must short-circuit

	cands := []Candidate{{Type: "VENDOR", Name: "ACME Blinds"}}
	res, _, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 1 || res[0].Decision != "match" || res[0].MatchID != "kent-N" {
		t.Fatalf("expected list-fallback match, got %+v", res)
	}
	if repo.gotProj != "proj" || repo.gotType != "VENDOR" {
		t.Errorf("repo not asked for the right project/type: %+v", repo)
	}
}

func TestResolver_NoCatalogYieldsLLMNew(t *testing.T) {
	repo := &fakeEntityRepo{} // empty everything
	llmReply := `[{"candidate_id":"cand-0","decision":"new","reason":"empty catalog"}]`
	fp := &fakeProvider{replies: []reply{{content: llmReply}}}
	r := NewResolver(fp, "fake", repo, fakeEmbedder)

	cands := []Candidate{{Type: "FACT", Name: "first ever fact"}}
	res, _, err := r.Resolve(context.Background(), "proj", cands)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(res) != 1 || res[0].Decision != "new" {
		t.Fatalf("expected new, got %+v", res)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3},
		{"flaw", "lawn", 2},
		{"a", "", 1},
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCosineSim(t *testing.T) {
	if got := cosineSim([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Errorf("identical vecs: cosine = %v, want ~1", got)
	}
	if got := cosineSim([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Errorf("orthogonal vecs: cosine = %v, want 0", got)
	}
	if got := cosineSim(nil, []float32{1, 0}); got != 0 {
		t.Errorf("nil vec must return 0, got %v", got)
	}
}

func TestAliasContains(t *testing.T) {
	payload := []byte(`["Postgres","PG","PostgreSQL"]`)
	if !aliasContains(payload, "PG") {
		t.Error("expected PG to be present")
	}
	if aliasContains(payload, "Mongo") {
		t.Error("expected Mongo to be absent")
	}
	if aliasContains(nil, "anything") {
		t.Error("nil payload must return false")
	}
}

func TestParseResolverOutput_AcceptsObjectWrapping(t *testing.T) {
	wrapped := `{"decisions":[{"candidate_id":"cand-0","decision":"match","match_id":"kent-1"}]}`
	parsed, err := parseResolverOutput(wrapped)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 || parsed[0].MatchID != "kent-1" {
		t.Errorf("object-wrap parse failed: %+v", parsed)
	}
}

func TestParseResolverOutput_StrictBareArrayPath(t *testing.T) {
	bare := `[{"candidate_id":"cand-7","decision":"NEW","reason":"x"}]`
	parsed, err := parseResolverOutput(bare)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Decision != "NEW" {
		t.Errorf("bare-array parse failed: %+v", parsed)
	}
	// normalize lowercases case-variants
	if got := normalizeDecision(parsed[0].Decision); got != "new" {
		t.Errorf("normalize lowercased mismatch, got %q", got)
	}
}

func TestNormalizeDecision_DefaultsToAmbiguous(t *testing.T) {
	cases := map[string]string{
		"match":     "match",
		"  Match  ": "match",
		"NEW":       "new",
		"???":       "ambiguous",
		"":          "ambiguous",
		"ambiguous": "ambiguous",
	}
	for in, want := range cases {
		if got := normalizeDecision(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolver_BuildsLLMUserMessageWithJSONArrays(t *testing.T) {
	// Sanity-check: when the LLM is invoked, the user message
	// contains valid JSON for both CANDIDATES and CATALOG.
	repo := &fakeEntityRepo{
		similar: []*persistence.KnowledgeEntity{
			{ID: "kent-1", CanonicalName: "Faraway Co", Aliases: []byte(`["FC"]`), Description: "test"},
		},
	}
	captured := &capturingProvider{reply: `[{"candidate_id":"cand-0","decision":"new"}]`}
	r := NewResolver(captured, "fake", repo, fakeEmbedder)
	_, _, err := r.Resolve(context.Background(), "proj", []Candidate{{Type: "VENDOR", Name: "ZeroCo"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(captured.userMsg, "CANDIDATES:") || !strings.Contains(captured.userMsg, "CATALOG:") {
		t.Errorf("user message missing labels: %q", captured.userMsg)
	}
	// Pull the CANDIDATES JSON section and ensure it parses.
	_, body, _ := strings.Cut(captured.userMsg, "CANDIDATES:\n")
	candsRaw, _, _ := strings.Cut(body, "\n\nCATALOG:")
	var arr []map[string]any
	if err := json.Unmarshal([]byte(candsRaw), &arr); err != nil {
		t.Errorf("CANDIDATES JSON did not parse: %v\nraw=%q", err, candsRaw)
	}
}
