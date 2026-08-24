package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// entity_mentions is the entity→chunk link, and the pipeline used to write it
// only when the extractor returned a usable character span.
//
// The span is a highlighting convenience; the ROW is the record of where the
// entity came from. §3.3 of the knowledge-graph LLD spells char_start as
// "nullable when extractor doesn't return offsets" — a span-less mention was
// always meant to be recorded.
//
// The cost was not cosmetic. Deletion paths decide whether an entity still
// belongs to a surviving chunk by asking whether a mention exists. Production
// 2026-08-21 held 3,796 entities with no mention, 522 of them still reachable
// through an edge citing a live chunk — live rows that read as stranded. It
// also causes OVER-deletion: erasing or evicting a different chunk that DID
// mention the entity destroys a row the survivor legitimately produced, because
// nothing links the survivor to it.

// failingMentionRepo makes Insert fail so the swallowed-error path is testable.
type failingMentionRepo struct {
	fakeMentionRepo
	err error
}

func (f *failingMentionRepo) Insert(_ context.Context, m *persistence.EntityMention) error {
	f.inserts = append(f.inserts, *m)
	return f.err
}

// A model that returns no offsets at all still gets its mention recorded.
func TestPipeline_recordsAMentionWhenTheExtractorReturnsNoOffsets(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-nospan", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL."}

	// No char_start / char_end keys: both default to 0, so the old
	// `CharEnd > CharStart` guard dropped the mention entirely.
	extractReply := `[{"type":"PERSON","name":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new","reason":"empty catalog"}]`

	p, _, _, mentRepo := newPipeline(t, extractReply, resolveReply, `[]`, `[]`)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	if m.MentionsWritten != 1 {
		t.Fatalf("MentionsWritten = %d, want 1 — an entity with no offsets is still "+
			"mentioned by this chunk, and the row is the only record of that",
			m.MentionsWritten)
	}
	if len(mentRepo.inserts) != 1 {
		t.Fatalf("expected one mention insert, got %+v", mentRepo.inserts)
	}
	got := mentRepo.inserts[0]
	if got.ChunkID != "chunk-nospan" || got.EntityID == "" {
		t.Errorf("the mention must link this chunk to the entity: %+v", got)
	}
	// Offsets are absent, not fabricated: a consumer can tell "position
	// unknown" from "at 0..0".
	if got.CharEnd != nil {
		t.Errorf("char_end must be NULL when no span was returned, got %v", *got.CharEnd)
	}
	if got.Surface != "" {
		t.Errorf("surface must be empty when no span was returned, got %q", got.Surface)
	}
	if m.MentionsWithoutSpan != 1 {
		t.Errorf("MentionsWithoutSpan = %d, want 1 — a deployment whose extractor "+
			"stops returning offsets should be able to see that", m.MentionsWithoutSpan)
	}
}

// Offsets the extractor got wrong are zeroed by validateCandidates. Those
// candidates kept their entity and lost their mention — the same severed link,
// reached by a different route.
func TestPipeline_recordsAMentionWhenOffsetsAreOutOfRange(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-badspan", ProjectID: "proj-1", Content: "short"}

	// char_end beyond the content length: validateCandidates keeps the entity
	// and drops the span.
	extractReply := `[{"type":"PERSON","name":"Vadim","char_start":0,"char_end":9000,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new","reason":"empty catalog"}]`

	p, entRepo, _, mentRepo := newPipeline(t, extractReply, resolveReply, `[]`, `[]`)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	if len(entRepo.inserted) != 1 {
		t.Fatalf("the entity itself must survive bad offsets, got %+v", entRepo.inserted)
	}
	if m.MentionsWritten != 1 || len(mentRepo.inserts) != 1 {
		t.Fatalf("a bad span must cost the OFFSETS, not the link: written=%d inserts=%+v",
			m.MentionsWritten, mentRepo.inserts)
	}
	if mentRepo.inserts[0].CharEnd != nil {
		t.Error("a rejected span must not be recorded as if it were real")
	}
}

// A usable span is still recorded in full — the fix must not flatten every
// mention to "position unknown".
func TestPipeline_keepsOffsetsWhenTheSpanIsValid(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-span", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL."}

	extractReply := `[{"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new","reason":"empty catalog"}]`

	p, _, _, mentRepo := newPipeline(t, extractReply, resolveReply, `[]`, `[]`)
	m, err := p.RunChunk(context.Background(), chunk)
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if len(mentRepo.inserts) != 1 {
		t.Fatalf("expected one mention, got %+v", mentRepo.inserts)
	}
	got := mentRepo.inserts[0]
	if got.CharStart != 0 || got.CharEnd == nil || *got.CharEnd != 5 || got.Surface != "Vadim" {
		t.Errorf("a valid span must survive intact: %+v", got)
	}
	if m.MentionsWithoutSpan != 0 {
		t.Errorf("MentionsWithoutSpan = %d, want 0", m.MentionsWithoutSpan)
	}
}

// The insert error was discarded with `if err == nil`, so a failed write was
// indistinguishable from a candidate that never had a mention — the same silent
// severing, from the other direction. An entity insert failure already fails the
// chunk; losing the mention now has the same consequences.
func TestPipeline_failsTheChunkWhenAMentionCannotBeWritten(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-mentionfail", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL."}

	extractReply := `[{"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"}]`
	resolveReply := `[{"candidate_id":"cand-0","decision":"new","reason":"empty catalog"}]`

	p, _, _, _ := newPipeline(t, extractReply, resolveReply, `[]`, `[]`)
	p.Mentions = &failingMentionRepo{err: errors.New("connection reset")}

	_, err := p.RunChunk(context.Background(), chunk)
	if err == nil {
		t.Fatal("a mention that could not be written must fail the chunk — the pass is " +
			"re-runnable, and a silently missing link makes a live entity look stranded")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the cause must survive: %v", err)
	}
	if !strings.Contains(err.Error(), "chunk-mentionfail") {
		t.Errorf("the error must name the chunk so the failure is actionable: %v", err)
	}
}

// The invariant `vornikctl memory backfill-entity-mentions` rests on.
//
// That repair reconstructs a lost entity→chunk link from
// knowledge_edges.source_chunks, reasoning that an edge citing a chunk proves
// the chunk resolved both endpoints. That is true only because THIS guard drops
// any proposal naming an entity outside the chunk's resolved set, and because
// pipeline.go upserts each edge with exactly one source chunk — its own — so
// the merge is a union of arrays whose members each satisfy the property.
//
// A reviewer argued the repair was unsound on the grounds that source_chunks
// accumulates across chunks. It does; the inference survives because every id
// entering it independently satisfies the property. Relax this guard and that
// stops being true, and the repair starts writing provenance links that were
// never real — so the dependency is pinned here rather than left as a comment
// in another package.
func TestValidateProposals_dropsEndpointsNotResolvedInThisChunk(t *testing.T) {
	resolved := []ResolvedEntity{{ID: "ent-in-chunk", Type: "PERSON", CanonicalName: "Vadim"}}

	kept, dropped, byReason := validateProposals([]EdgeProposal{
		// Both endpoints resolved here — the only shape that may reach an edge.
		{From: "ent-in-chunk", To: "ent-in-chunk-2", Predicate: "DEPENDS_ON", Evidence: "e"},
		// An endpoint this chunk never resolved. If this survived, the edge would
		// cite a chunk that did not mention it.
		{From: "ent-in-chunk", To: "ent-from-another-chunk", Predicate: "DEPENDS_ON", Evidence: "e"},
		{From: "ent-from-another-chunk", To: "ent-in-chunk", Predicate: "DEPENDS_ON", Evidence: "e"},
	}, resolved, "content")

	if len(kept) != 0 {
		t.Errorf("no proposal here has both endpoints resolved in this chunk; kept %+v", kept)
	}
	if dropped != 3 {
		t.Errorf("dropped %d, want 3", dropped)
	}
	if byReason[DropReasonUnknownTo] != 2 || byReason[DropReasonUnknownFrom] != 1 {
		t.Errorf("an unresolved endpoint must be dropped by name, got %v", byReason)
	}
}

// And the other half: an edge is upserted citing exactly ONE chunk, its own.
// Together with the guard above, that is what makes every id in a merged
// source_chunks array a chunk that resolved both endpoints.
func TestPipeline_upsertsEdgesCitingOnlyTheChunkThatProducedThem(t *testing.T) {
	chunk := ChunkInput{ID: "chunk-src", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL 16 for the ledger."}

	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5,"surface":"Vadim"},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25,"surface":"PostgreSQL 16"}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"new","reason":"empty catalog"},
	  {"candidate_id":"cand-1","decision":"new","reason":"empty catalog"}
	]`
	relReply := `[{"from":"kent-new-person-1","to":"kent-new-technology-2","predicate":"DEPENDS_ON","evidence":"Vadim chose PostgreSQL 16"}]`
	valReply := `[{"id":"prop-0","score":0.95,"reason":"explicit"}]`

	p, _, edgeRepo, _ := newPipeline(t, extractReply, resolveReply, relReply, valReply)
	if _, err := p.RunChunk(context.Background(), chunk); err != nil {
		t.Fatalf("RunChunk: %v", err)
	}

	if len(edgeRepo.upserts) != 1 {
		t.Fatalf("expected one edge, got %+v", edgeRepo.upserts)
	}
	got := edgeRepo.upserts[0].SourceChunks
	if len(got) != 1 || got[0] != chunk.ID {
		t.Errorf("an edge must cite exactly the chunk that produced it, got %v", got)
	}
}
