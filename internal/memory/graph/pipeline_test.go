package graph

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
)

// fakeEdgeRepo records UpsertEdge calls. Other methods panic so
// any unexpected pipeline call surfaces loudly.
type fakeEdgeRepo struct {
	upserts []persistence.KnowledgeEdge
}

func (f *fakeEdgeRepo) UpsertEdge(_ context.Context, e *persistence.KnowledgeEdge) error {
	if e.ID == "" {
		e.ID = "kedge-" + e.Predicate + "-" + e.FromEntity + "-" + e.ToEntity
	}
	f.upserts = append(f.upserts, *e)
	return nil
}
func (f *fakeEdgeRepo) Get(context.Context, string) (*persistence.KnowledgeEdge, error) {
	panic("not used")
}
func (f *fakeEdgeRepo) List(context.Context, persistence.KnowledgeEdgeFilter) ([]*persistence.KnowledgeEdge, error) {
	panic("not used")
}
func (f *fakeEdgeRepo) EdgesForEntity(context.Context, string, int) ([]*persistence.KnowledgeEdge, error) {
	panic("not used")
}
func (f *fakeEdgeRepo) UpdateLifecycle(context.Context, string, string) error {
	panic("not used")
}
func (f *fakeEdgeRepo) DropChunkFromSources(context.Context, string) (int, error) {
	panic("not used")
}

type fakeMentionRepo struct {
	inserts []persistence.EntityMention
}

func (f *fakeMentionRepo) Insert(_ context.Context, m *persistence.EntityMention) error {
	f.inserts = append(f.inserts, *m)
	return nil
}
func (f *fakeMentionRepo) ListByEntity(context.Context, string, int) ([]*persistence.EntityMention, error) {
	panic("not used")
}
func (f *fakeMentionRepo) ListByChunk(context.Context, string) ([]*persistence.EntityMention, error) {
	panic("not used")
}
func (f *fakeMentionRepo) DeleteForChunk(context.Context, string) error { panic("not used") }

// fakeEntityRepoForPipeline extends the resolver fake with
// Insert + AddAlias call recording so pipeline tests can assert
// on entity creation.
type fakeEntityRepoForPipeline struct {
	fakeEntityRepo
	inserted   []persistence.KnowledgeEntity
	addAliased []struct{ ID, Alias string }
	idCounter  atomic.Int32
}

func (f *fakeEntityRepoForPipeline) Insert(_ context.Context, e *persistence.KnowledgeEntity) error {
	if e.ID == "" {
		n := f.idCounter.Add(1)
		e.ID = "kent-new-" + strings.ToLower(e.Type) + "-"
		switch n {
		case 1:
			e.ID += "1"
		case 2:
			e.ID += "2"
		case 3:
			e.ID += "3"
		default:
			e.ID += "n"
		}
	}
	f.inserted = append(f.inserted, *e)
	return nil
}
func (f *fakeEntityRepoForPipeline) AddAlias(_ context.Context, id, alias string) error {
	f.addAliased = append(f.addAliased, struct{ ID, Alias string }{id, alias})
	return nil
}

// scriptedFake returns a fixed reply per stage. Useful when one
// pipeline run hits multiple stages each backed by its own
// provider.
func scriptedFake(content string) *fakeProvider {
	return &fakeProvider{replies: []reply{{content: content}}}
}

func newPipeline(t *testing.T,
	extractReply, resolveReply, relReply, valReply string,
) (*Pipeline, *fakeEntityRepoForPipeline, *fakeEdgeRepo, *fakeMentionRepo) {
	t.Helper()
	entRepo := &fakeEntityRepoForPipeline{}
	edgeRepo := &fakeEdgeRepo{}
	mentRepo := &fakeMentionRepo{}

	ex := NewExtractor(scriptedFake(extractReply), "extract-fake")
	res := NewResolver(scriptedFake(resolveReply), "resolve-fake", entRepo, fakeEmbedder)
	rel := NewRelationshipExtractor(scriptedFake(relReply), "rel-fake")
	val := NewValidator(scriptedFake(valReply), "val-fake")

	p := &Pipeline{
		Extractor: ex, Resolver: res, Relations: rel, Validator: val,
		Entities: entRepo, Edges: edgeRepo, Mentions: mentRepo,
		Embedder: fakeEmbedder,
	}
	return p, entRepo, edgeRepo, mentRepo
}

func TestPipeline_HappyPath_NewEntitiesAndEdges(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-1", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL 16 for the new ledger build."}

	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25,"surface":"PostgreSQL 16"}
	]`
	// Empty repo → resolver hits LLM for both, marks both as new.
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new","reason":"empty catalog"},
	  {"candidate_id":"cand-1","decision":"new","reason":"empty catalog"}
	]`
	// Relationship stage proposes one DEPENDS_ON edge.
	relReply := `[{"from":"kent-new-person-1","to":"kent-new-technology-2","predicate":"DEPENDS_ON","evidence":"Vadim chose PostgreSQL 16"}]`
	valReply := `[{"id":"prop-0","score":0.95,"reason":"explicit"}]`

	p, entRepo, edgeRepo, mentRepo := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	if m.EntitiesCreated != 2 {
		t.Errorf("expected 2 entities created, got %d (inserts=%+v)", m.EntitiesCreated, entRepo.inserted)
	}
	if m.EntitiesMatched != 0 || m.EntitiesAmbiguous != 0 {
		t.Errorf("unexpected match/ambiguous counts: matched=%d amb=%d", m.EntitiesMatched, m.EntitiesAmbiguous)
	}
	if m.MentionsWritten != 2 {
		t.Errorf("expected 2 mentions, got %d (%+v)", m.MentionsWritten, mentRepo.inserts)
	}
	if m.EdgesUpserted != 1 || m.EdgesDropped != 0 {
		t.Errorf("expected 1 edge upserted, got upserted=%d dropped=%d (edges=%+v)", m.EdgesUpserted, m.EdgesDropped, edgeRepo.upserts)
	}
	if len(edgeRepo.upserts) != 1 {
		t.Fatalf("edge repo got %d edges, want 1", len(edgeRepo.upserts))
	}
	e := edgeRepo.upserts[0]
	if e.Predicate != "DEPENDS_ON" || e.FromEntity != "kent-new-person-1" || e.ToEntity != "kent-new-technology-2" {
		t.Errorf("edge content unexpected: %+v", e)
	}
	if e.Faithfulness == nil || *e.Faithfulness < 0.94 {
		t.Errorf("faithfulness not stamped: %+v", e.Faithfulness)
	}
	if len(e.SourceChunks) != 1 || e.SourceChunks[0] != "chunk-1" {
		t.Errorf("source_chunks not stamped: %+v", e.SourceChunks)
	}
	// The fake provider stamps Model="fake" in its responses; the
	// orchestrator preserves the served-model name (matches
	// production semantics — see judge.go's resp.Model handling).
	if e.ExtractedBy != "fake" {
		t.Errorf("extracted_by should reflect served model name, got %q", e.ExtractedBy)
	}
}

func TestPipeline_MatchedEntityRecordsAlias(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-2", ProjectID: "proj-1", Content: "Postgres is fast."}

	extractReply := `[{"type":"TECHNOLOGY","name":"Postgres","char_start":0,"char_end":8,"surface":"Postgres"}]`
	// Resolver short-circuits via the existing catalog hit.
	resolveReply := `` // unused — short-circuit fires before LLM
	relReply := `[]`
	valReply := `[]`

	p, entRepo, _, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	// Seed the catalog so the short-circuit fires.
	entRepo.similar = []*persistence.KnowledgeEntity{
		{ID: "kent-existing-1", CanonicalName: "Postgres", Type: "TECHNOLOGY"},
	}

	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EntitiesMatched != 1 || m.EntitiesCreated != 0 {
		t.Errorf("expected matched=1 created=0, got matched=%d created=%d", m.EntitiesMatched, m.EntitiesCreated)
	}
	// No new alias to add (canonical_name == candidate.Name).
	if len(entRepo.addAliased) != 0 {
		t.Errorf("did not expect alias add, got %+v", entRepo.addAliased)
	}
}

func TestPipeline_AmbiguousResolvesToQuarantined(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-3", ProjectID: "proj-1", Content: "ZeroCo signed a contract with Initech."}

	extractReply := `[
	  {"type":"VENDOR","name":"ZeroCo","char_start":0,"char_end":6,"surface":"ZeroCo"},
	  {"type":"VENDOR","name":"Initech","char_start":29,"char_end":36,"surface":"Initech"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"ambiguous","reason":"two ZeroCos in catalog"},
	  {"candidate_id":"cand-1","decision":"new","reason":"first sighting"}
	]`
	relReply := `[]`
	valReply := `[]`

	p, entRepo, _, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EntitiesAmbiguous != 1 {
		t.Errorf("expected 1 ambiguous, got %d", m.EntitiesAmbiguous)
	}
	if m.EntitiesCreated != 1 {
		t.Errorf("expected 1 new, got %d", m.EntitiesCreated)
	}
	// Verify the ambiguous entity landed with lifecycle=quarantined.
	var amb *persistence.KnowledgeEntity
	for i, e := range entRepo.inserted {
		if e.CanonicalName == "ZeroCo" {
			amb = &entRepo.inserted[i]
		}
	}
	if amb == nil {
		t.Fatalf("ambiguous entity not inserted: %+v", entRepo.inserted)
	}
	if amb.LifecycleState != "quarantined" {
		t.Errorf("ambiguous should be quarantined, got %q", amb.LifecycleState)
	}
}

func TestPipeline_EmptyExtractionShortCircuits(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-empty", ProjectID: "proj-1", Content: "nothing meaningful here"}
	extractReply := `[]`
	p, _, edgeRepo, mentRepo := newPipeline(t, extractReply, "", "", "")
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EntitiesCreated+m.EntitiesMatched+m.EntitiesAmbiguous != 0 {
		t.Errorf("expected no entity ops, got %+v", m)
	}
	if len(edgeRepo.upserts) != 0 || len(mentRepo.inserts) != 0 {
		t.Errorf("expected no DB writes")
	}
	if m.Resolve != nil || m.Relations != nil || m.Validate != nil {
		t.Errorf("expected later stages not invoked, got %+v", m)
	}
}

func TestPipeline_LowFaithfulnessEdgeDropped(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-4", ProjectID: "proj-1", Content: "Vadim and PostgreSQL 16 were both mentioned."}

	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":10,"char_end":23,"surface":"PostgreSQL 16"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new"},
	  {"candidate_id":"cand-1","decision":"new"}
	]`
	relReply := `[{"from":"kent-new-person-1","to":"kent-new-technology-2","predicate":"DEPENDS_ON","evidence":"Vadim and PostgreSQL 16 were both mentioned"}]`
	// Validator scores below threshold → edge drops.
	valReply := `[{"id":"prop-0","score":0.4,"reason":"only co-mention, no dependency stated"}]`

	p, _, edgeRepo, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EdgesUpserted != 0 || m.EdgesDropped != 1 {
		t.Errorf("expected 0 upserts, 1 drop; got upserted=%d dropped=%d", m.EdgesUpserted, m.EdgesDropped)
	}
	if len(edgeRepo.upserts) != 0 {
		t.Errorf("dropped edge should not have been upserted, got %+v", edgeRepo.upserts)
	}
}

func TestPipeline_RejectsInvalidWiring(t *testing.T) {
	p := &Pipeline{}
	_, err := p.RunChunk(context.Background(), ChunkInput{ID: "x", ProjectID: "y", Content: "z"})
	if err == nil || !strings.Contains(err.Error(), "Extractor not wired") {
		t.Errorf("expected wiring error, got %v", err)
	}
}

func TestPipeline_RejectsMissingChunkFields(t *testing.T) {
	p, _, _, _ := newPipeline(t, "[]", "", "", "")
	_, err := p.RunChunk(context.Background(), ChunkInput{ID: "", ProjectID: "p", Content: "x"})
	if err == nil {
		t.Error("expected error on empty chunk id")
	}
	_, err = p.RunChunk(context.Background(), ChunkInput{ID: "c", ProjectID: "", Content: "x"})
	if err == nil {
		t.Error("expected error on empty project id")
	}
}

func TestPipeline_SameChunkDedupesNewDecisions(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-dup", ProjectID: "proj-1", Content: "Vadim met Vadim again, both Vadim."}

	// Three candidates, all "Vadim Grinco" → resolver tags each
	// as new. Without dedup the second insert would hit
	// ErrDuplicateKey on the unique (project_id, type, name)
	// constraint.
	extractReply := `[
	  {"type":"PERSON","name":"Vadim Grinco","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"PERSON","name":"Vadim Grinco","char_start":10,"char_end":15,"surface":"Vadim"},
	  {"type":"PERSON","name":"Vadim Grinco","char_start":21,"char_end":26,"surface":"Vadim"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new"},
	  {"candidate_id":"cand-1","decision":"new"},
	  {"candidate_id":"cand-2","decision":"new"}
	]`
	p, entRepo, _, mentRepo := newPipeline(t, extractReply, resolveReply, "[]", "[]")
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EntitiesCreated != 1 {
		t.Errorf("expected 1 entity created (deduped), got %d (inserts=%+v)", m.EntitiesCreated, entRepo.inserted)
	}
	if m.EntitiesMatched != 2 {
		t.Errorf("expected 2 same-chunk dup hits classified as match, got %d", m.EntitiesMatched)
	}
	// Three mentions should all reference the SAME entity id.
	if len(mentRepo.inserts) != 3 {
		t.Errorf("expected 3 mentions, got %d", len(mentRepo.inserts))
	}
	for _, mention := range mentRepo.inserts {
		if mention.EntityID == "" || mention.EntityID != mentRepo.inserts[0].EntityID {
			t.Errorf("mention entity ids diverged: %+v", mentRepo.inserts)
			break
		}
	}
}

// duplicateKeyEntityRepo simulates the cross-chunk race: the
// first Insert returns ErrDuplicateKey; GetByCanonical surfaces
// the existing row. Behavior on subsequent Inserts: succeeds.
type duplicateKeyEntityRepo struct {
	fakeEntityRepoForPipeline
	dupOnce  bool
	existing *persistence.KnowledgeEntity
}

func (d *duplicateKeyEntityRepo) Insert(ctx context.Context, e *persistence.KnowledgeEntity) error {
	if !d.dupOnce {
		d.dupOnce = true
		return persistence.ErrDuplicateKey
	}
	return d.fakeEntityRepoForPipeline.Insert(ctx, e)
}
func (d *duplicateKeyEntityRepo) GetByCanonical(_ context.Context, _, _, _ string) (*persistence.KnowledgeEntity, error) {
	return d.existing, nil
}

func TestPipeline_RecoversFromCrossChunkDuplicateKey(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-x", ProjectID: "proj-1", Content: "Vadim is here."}
	extractReply := `[{"type":"PERSON","name":"Vadim Grinco","char_start":0,"char_end":5,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new"}]`

	dupRepo := &duplicateKeyEntityRepo{
		existing: &persistence.KnowledgeEntity{ID: "kent-existing-99", CanonicalName: "Vadim Grinco", Type: "PERSON"},
	}
	edgeRepo := &fakeEdgeRepo{}
	mentRepo := &fakeMentionRepo{}
	p := &Pipeline{
		Extractor: NewExtractor(scriptedFake(extractReply), "ex"),
		Resolver:  NewResolver(scriptedFake(resolveReply), "res", dupRepo, fakeEmbedder),
		Relations: NewRelationshipExtractor(scriptedFake("[]"), "rel"),
		Validator: NewValidator(scriptedFake("[]"), "val"),
		Entities:  dupRepo,
		Edges:     edgeRepo,
		Mentions:  mentRepo,
		Embedder:  fakeEmbedder,
	}
	_, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if len(mentRepo.inserts) != 1 || mentRepo.inserts[0].EntityID != "kent-existing-99" {
		t.Errorf("expected mention to reference recovered entity id, got %+v", mentRepo.inserts)
	}
}

func TestPipeline_SingleEntitySkipsRelationship(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-solo", ProjectID: "proj-1", Content: "Just Vadim."}
	extractReply := `[{"type":"PERSON","name":"Vadim","char_start":5,"char_end":10,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new"}]`
	// relReply / valReply unused — pipeline short-circuits.
	p, _, edgeRepo, _ := newPipeline(t, extractReply, resolveReply, "", "")
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.EntitiesCreated != 1 {
		t.Errorf("expected 1 entity, got %d", m.EntitiesCreated)
	}
	if m.Relations != nil || m.Validate != nil {
		t.Errorf("expected stage 3+4 skipped (1 entity), got %+v", m)
	}
	if len(edgeRepo.upserts) != 0 {
		t.Errorf("expected no edges, got %+v", edgeRepo.upserts)
	}
}

// TestPipeline_EmitsPerReasonDropCounter end-to-end-validates the
// Prometheus emission introduced alongside the per-reason
// DropsByReason map. Drives a chunk where the relationship LLM
// emits four proposals — one good plus one per drop reason —
// and asserts each label on RelationshipDroppedTotal increments
// exactly once. Without this test, the metric could quietly stop
// emitting on a future refactor (drop counters are easy to break
// silently because their absence shows as a blank Grafana panel).
func TestPipeline_EmitsPerReasonDropCounter(t *testing.T) {
	chunk := ChunkInput{
		ID:        "chunk-emit-drops",
		ProjectID: "proj-1",
		Content:   "ACME and Globex both attended the meeting.",
	}

	extractReply := `[
	  {"type":"VENDOR","name":"ACME","char_start":0,"char_end":4,"surface":"ACME"},
	  {"type":"VENDOR","name":"Globex","char_start":9,"char_end":15,"surface":"Globex"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new","reason":"empty catalog"},
	  {"candidate_id":"cand-1","decision":"new","reason":"empty catalog"}
	]`
	// Mix of one accepted edge + four drop reasons. The exact
	// entity IDs match newPipeline's `kent-new-vendor-<n>`
	// pattern.
	relReply := `[
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-2","predicate":"RELATES_TO","evidence":"ACME and Globex both attended"},
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-1","predicate":"RELATES_TO","evidence":"ACME and Globex both attended"},
	  {"from":"kent-new-vendor-1","to":"kent-MYSTERY","predicate":"RELATES_TO","evidence":"ACME and Globex both attended"},
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-2","predicate":"FABRICATED_PREDICATE","evidence":"ACME and Globex both attended"},
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-2","predicate":"RELATES_TO","evidence":"a quote that is not in the chunk"}
	]`
	valReply := `[{"id":"prop-0","score":0.9,"reason":"strong evidence"}]`

	p, _, _, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)

	// Wire a private registry + Metrics so this test doesn't
	// pollute the global registerer or other tests' counters.
	reg := prometheus.NewRegistry()
	p.Metrics = NewMetrics(reg)

	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	// PipelineMetrics carries the in-process map; Prometheus
	// counter mirrors it.
	if m.Relations == nil {
		t.Fatal("Relations metrics missing")
	}
	wantReasons := map[string]float64{
		DropReasonSelfLoop:           1,
		DropReasonUnknownTo:          1,
		DropReasonUnknownPredicate:   1,
		DropReasonEvidenceNotInChunk: 1,
	}
	for reason, want := range wantReasons {
		got := testutil.ToFloat64(p.Metrics.RelationshipDroppedTotal.WithLabelValues(reason))
		if got != want {
			t.Errorf("RelationshipDroppedTotal[%s] = %v, want %v (DropsByReason=%v)",
				reason, got, want, m.Relations.DropsByReason)
		}
	}

	// Sanity: the one accepted proposal made it through both
	// the cheap pre-validator pass AND the LLM faithfulness
	// stage, so EdgesUpserted = 1.
	if m.EdgesUpserted != 1 {
		t.Errorf("expected 1 edge upserted, got %d", m.EdgesUpserted)
	}

	// Reasons not exercised in this chunk should stay zero so
	// dashboards don't show phantom drops on a per-reason
	// query.
	for _, unused := range []string{DropReasonEmptyEndpoint, DropReasonEmptyEvidence, DropReasonUnknownFrom, DropReasonDuplicateTriple} {
		got := testutil.ToFloat64(p.Metrics.RelationshipDroppedTotal.WithLabelValues(unused))
		if got != 0 {
			t.Errorf("unexercised reason %q increased to %v", unused, got)
		}
	}
}

// TestPipeline_EmitsExtractorOutcome — closes the end-to-end loop
// for the per-chunk extractor outcome counter (commit follows the
// 2026-05-25 audit's "67% empty extraction" finding). Drives a
// chunk where the extractor returns [] (the dominant failure
// mode), then verifies the empty_response label increments and
// the other two labels stay at zero. Without this test, future
// refactors of the emit path could silently break the metric and
// dashboards would go blank.
func TestPipeline_EmitsExtractorOutcome(t *testing.T) {
	chunk := ChunkInput{
		ID:        "chunk-empty-extract",
		ProjectID: "proj-1",
		Content:   "A chunk the LLM returns nothing for.",
	}

	// Extractor returns []; pipeline short-circuits before
	// resolver / relationship / validator.
	p, _, _, _ := newPipeline(t, "[]", "", "", "")
	reg := prometheus.NewRegistry()
	p.Metrics = NewMetrics(reg)

	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.Extract == nil {
		t.Fatal("Extract metrics missing")
	}
	if m.Extract.Outcome != ExtractOutcomeEmptyResponse {
		t.Fatalf("Outcome = %q, want %q", m.Extract.Outcome, ExtractOutcomeEmptyResponse)
	}

	// Counter assertions.
	got := testutil.ToFloat64(p.Metrics.ExtractorOutcomesTotal.WithLabelValues(ExtractOutcomeEmptyResponse))
	if got != 1 {
		t.Errorf("ExtractorOutcomesTotal[empty_response] = %v, want 1", got)
	}
	for _, unused := range []string{ExtractOutcomeProduced, ExtractOutcomeDroppedAllInvalid} {
		got := testutil.ToFloat64(p.Metrics.ExtractorOutcomesTotal.WithLabelValues(unused))
		if got != 0 {
			t.Errorf("unexercised label %q increased to %v", unused, got)
		}
	}
}

// TestPipeline_EmitsValidatorDropsByReason mirrors the relationship-
// stage emission test for the validator side. Drives a chunk where
// the relationship LLM proposes two edges, the validator scores
// only one (the other is "missing"), and the kept one falls below
// threshold. Asserts both ValidatorDropsByReasonTotal labels
// increment exactly once and the scalar ValidatorDroppedTotal
// stays in sync.
func TestPipeline_EmitsValidatorDropsByReason(t *testing.T) {
	chunk := ChunkInput{
		ID:        "chunk-validator-drops",
		ProjectID: "proj-1",
		Content:   "ACME and Globex both attended the meeting.",
	}

	extractReply := `[
	  {"type":"VENDOR","name":"ACME","char_start":0,"char_end":4,"surface":"ACME"},
	  {"type":"VENDOR","name":"Globex","char_start":9,"char_end":15,"surface":"Globex"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new","reason":"empty catalog"},
	  {"candidate_id":"cand-1","decision":"new","reason":"empty catalog"}
	]`
	// Two valid edge proposals — both pass the cheap pre-
	// validator, both reach the LLM validator stage.
	relReply := `[
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-2","predicate":"RELATES_TO","evidence":"ACME and Globex both attended"},
	  {"from":"kent-new-vendor-1","to":"kent-new-vendor-2","predicate":"MENTIONED_IN","evidence":"ACME and Globex both attended"}
	]`
	// Validator: scores prop-0 below threshold (0.3 < 0.7),
	// omits prop-1 entirely. Two distinct drop reasons in one
	// run.
	valReply := `[{"id":"prop-0","score":0.3,"reason":"weak"}]`

	p, _, _, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	reg := prometheus.NewRegistry()
	p.Metrics = NewMetrics(reg)

	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.Validate == nil {
		t.Fatal("Validate metrics missing")
	}

	wantReasons := map[string]float64{
		ValidatorDropReasonBelowThreshold: 1,
		ValidatorDropReasonMissingScore:   1,
	}
	for reason, want := range wantReasons {
		got := testutil.ToFloat64(p.Metrics.ValidatorDropsByReasonTotal.WithLabelValues(reason))
		if got != want {
			t.Errorf("ValidatorDropsByReasonTotal[%s] = %v, want %v (DropsByReason=%v)",
				reason, got, want, m.Validate.DropsByReason)
		}
	}

	// Scalar ValidatorDroppedTotal increments once per Dropped
	// edge inside pipeline.go's post-validator loop. Two drops
	// here → counter == 2. This invariant is the back-compat
	// guarantee for the existing Grafana panel.
	totalScalar := testutil.ToFloat64(p.Metrics.ValidatorDroppedTotal)
	if totalScalar != 2 {
		t.Errorf("ValidatorDroppedTotal scalar = %v, want 2", totalScalar)
	}

	// And: total = sum of label values (cross-counter invariant).
	var sumLabels float64
	for _, want := range wantReasons {
		sumLabels += want
	}
	if sumLabels != totalScalar {
		t.Errorf("label sum %v != scalar %v — counters drifted", sumLabels, totalScalar)
	}
}
