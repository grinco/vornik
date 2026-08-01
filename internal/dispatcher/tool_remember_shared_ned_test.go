package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/memory/ned"
	"vornik.io/vornik/internal/persistence"
)

// SLICE 4 of the chat memory-write design (§6): the shared-scope pre-commit NED
// gate wired into the write path. These tests drive ToolExecutor.remember with
// in-memory fakes and a REAL ned.Gate (fed fake extractor/resolver), so the
// type-level token guardrail is exercised end-to-end — the write path can only
// be reached through a proceed verdict, which only ned.Gate can mint.

// fakeNEDExtractor / fakeNEDResolver implement the exported ned.Extractor /
// ned.Resolver seams so a test can script the four NED outcomes.
type fakeNEDExtractor struct {
	cands []graph.Candidate
	err   error
}

func (f *fakeNEDExtractor) Extract(_ context.Context, _ string) ([]graph.Candidate, *graph.ExtractMetrics, error) {
	return f.cands, &graph.ExtractMetrics{Model: "test"}, f.err
}

type fakeNEDResolver struct {
	resns []graph.Resolution
	err   error
}

func (f *fakeNEDResolver) Resolve(_ context.Context, _ string, _ []graph.Candidate) ([]graph.Resolution, *graph.ResolveMetrics, error) {
	return f.resns, &graph.ResolveMetrics{Model: "test"}, f.err
}

func nedGate(ex *fakeNEDExtractor, res *fakeNEDResolver) *ned.Gate {
	return &ned.Gate{Extractor: ex, Resolver: res}
}

func personCand(name string) graph.Candidate {
	return graph.Candidate{Type: persistence.EntityTypePerson, Name: name}
}

// sharedNEDExecutor is a fully-wired shared-write executor: gate + confirmations
// + audit + pipeline + linker.
func sharedNEDExecutor(confirms *fakeConfirmRepo, audit *fakeAuditRepo, w ChatMemoryWriter, linker DataSubjectLinker, gate *ned.Gate) *ToolExecutor {
	return &ToolExecutor{
		memoryWrite:    &stubMemoryWriteGate{allow: map[string]bool{"slack|sess": true}},
		memoryConfirms: confirms,
		memoryAudit:    audit,
		chatMemory:     w,
		dataSubjects:   linker,
		sharedNED:      gate,
	}
}

// seedAck seeds an acknowledged pending row for `content`, the state the
// receiver's ack hook would have produced.
func seedAck(confirms *fakeConfirmRepo, content string) {
	now := time.Now()
	confirms.seedAcknowledged(persistence.ChatMemoryWriteConfirmation{
		Channel: "slack", SessionID: "sess", ContentFingerprint: sharedWriteFingerprint(content),
		Scope: string(memoryScopeShared), OperatorID: "slack:UALICE",
		ProposedAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(12 * time.Minute),
	}, now.Add(-2*time.Minute))
}

// Shared end-to-end: NED resolves the named person to a `match`, so the write
// persists a chat_memory chunk AND links the resolved third party to every
// chunk (D4.1), audits, and one-shot-deletes the pending row.
func TestRememberSharedNED_MatchPersistsAndLinksThirdParty(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_s1", ChunkIDs: []string{"chunk-a", "chunk-b"},
		Stats: memory.IngestStats{Admitted: 1},
	}}
	linker := &fakeLinker{} // FindSubjectByIdentifier returns "" → creates subject
	gate := nedGate(
		&fakeNEDExtractor{cands: []graph.Candidate{personCand("Bob")}},
		&fakeNEDResolver{resns: []graph.Resolution{{Decision: "match", MatchID: "ent-bob"}}},
	)
	te := sharedNEDExecutor(confirms, audit, w, linker, gate)

	content := "Bob owns the Q4 rollout"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	if w.ingestCalls != 1 {
		t.Fatalf("IngestChatMemory calls = %d, want 1", w.ingestCalls)
	}
	if !strings.Contains(strings.ToLower(res.Content), "saved to shared") {
		t.Errorf("must report the shared save truthfully; got: %s", res.Content)
	}
	// One kg_entity link per chunk, targeting project_memory_chunks at the
	// SourceKGExtraction ceiling, marked shared.
	if len(linker.links) != 2 {
		t.Fatalf("links = %d, want 2 (one per chunk)", len(linker.links))
	}
	for _, l := range linker.links {
		if l.Table != datasubject.TableProjectMemoryChunks || l.Source != datasubject.SourceKGExtraction ||
			l.Confidence != datasubject.ConfidencePossible || l.Exclusivity != datasubject.SharedRow {
			t.Errorf("third-party link wrong: %+v", l)
		}
	}
	if !linker.created || len(linker.identifiers) != 1 || linker.identifiers[0].Kind != datasubject.KindKGEntity {
		t.Errorf("a new subject + kg_entity identifier must be created; created=%v ids=%+v", linker.created, linker.identifiers)
	}
	if audit.count() != 1 {
		t.Errorf("exactly one audit row must be written; got %d", audit.count())
	}
	if _, ok := confirms.get("slack"); ok {
		t.Error("the pending row must be deleted after a granted+persisted write (one-shot)")
	}
	if len(w.deleted) != 0 {
		t.Errorf("no compensation delete on success; got %v", w.deleted)
	}
}

// A shared write with NO third party (no PERSON extracted) still persists, and
// records no data-subject link (there is nobody to link).
func TestRememberSharedNED_NoPersonPersistsWithoutLink(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_s2", ChunkIDs: []string{"chunk-a"}, Stats: memory.IngestStats{Admitted: 1},
	}}
	linker := &fakeLinker{}
	gate := nedGate(&fakeNEDExtractor{}, &fakeNEDResolver{})
	te := sharedNEDExecutor(confirms, audit, w, linker, gate)

	content := "the roadmap moved to Q4"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	if w.ingestCalls != 1 || !strings.Contains(strings.ToLower(res.Content), "saved to shared") {
		t.Fatalf("a no-person shared write must persist; ingestCalls=%d res=%s", w.ingestCalls, res.Content)
	}
	if len(linker.links) != 0 {
		t.Errorf("no data-subject link when nobody is named; got %d", len(linker.links))
	}
}

// NED BLOCK (`new`): the write is refused, the refusal NAMES the person, and
// ZERO rows are written — the gate precedes any insert (C1). The pending
// confirmation is left in place so the user can rephrase.
func TestRememberSharedNED_NewBlocksNamesPersonZeroRows(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	w := &fakeChatMemoryWriter{}
	linker := &fakeLinker{}
	gate := nedGate(
		&fakeNEDExtractor{cands: []graph.Candidate{personCand("Carol")}},
		&fakeNEDResolver{resns: []graph.Resolution{{Decision: "new"}}},
	)
	te := sharedNEDExecutor(confirms, audit, w, linker, gate)

	content := "Carol is joining the security team"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	if !strings.Contains(res.Content, "Carol") {
		t.Errorf("the refusal must name the detected person; got: %s", res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "personal profile") {
		t.Errorf("the refusal must offer the personal-profile path (§6.2.1); got: %s", res.Content)
	}
	// ZERO rows: nothing ingested, nothing audited, and the pending row survives.
	if w.ingestCalls != 0 {
		t.Errorf("a NED block must write ZERO rows; ingestCalls=%d", w.ingestCalls)
	}
	if audit.count() != 0 {
		t.Errorf("a NED block must write no audit row; got %d", audit.count())
	}
	if _, ok := confirms.get("slack"); !ok {
		t.Error("the pending confirmation must survive a NED block so the user can rephrase")
	}
}

// NED ERROR (extract/resolve failure): fail CLOSED — refuse, distinct from a
// block ("couldn't verify"), zero rows written (D6.3, review suggestion 5).
func TestRememberSharedNED_ErrorFailsClosedZeroRows(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	w := &fakeChatMemoryWriter{}
	gate := nedGate(&fakeNEDExtractor{err: errors.New("model timeout")}, &fakeNEDResolver{})
	te := sharedNEDExecutor(confirms, audit, w, &fakeLinker{}, gate)

	content := "something that might name a person"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	if !strings.Contains(strings.ToLower(res.Content), "couldn't verify") {
		t.Errorf("an NED error must fail closed with a 'couldn't verify' refusal; got: %s", res.Content)
	}
	if w.ingestCalls != 0 || audit.count() != 0 {
		t.Errorf("an NED error must write ZERO rows; ingestCalls=%d audit=%d", w.ingestCalls, audit.count())
	}
	if _, ok := confirms.get("slack"); !ok {
		t.Error("the pending confirmation must survive a NED error")
	}
}

// D4.1a compensation on the shared path: NED matches, the chunk is written, but
// the data-subject link fails → the chunk is compensated away (DeleteChatMemory),
// no audit is written, the pending row survives, and the tool does not imply a save.
func TestRememberSharedNED_LinkFailureCompensates(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_s3", ChunkIDs: []string{"chunk-a"}, Stats: memory.IngestStats{Admitted: 1},
	}}
	linker := &fakeLinker{addLinkErr: errors.New("db down")}
	gate := nedGate(
		&fakeNEDExtractor{cands: []graph.Candidate{personCand("Bob")}},
		&fakeNEDResolver{resns: []graph.Resolution{{Decision: "match", MatchID: "ent-bob"}}},
	)
	te := sharedNEDExecutor(confirms, audit, w, linker, gate)

	content := "Bob signed off on the release"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	if len(w.deleted) != 1 || w.deleted[0] != "chatmem_s3" {
		t.Fatalf("a shared link failure must compensate the chunk away; deleted=%v", w.deleted)
	}
	if audit.count() != 0 {
		t.Errorf("no audit row when the write was compensated; got %d", audit.count())
	}
	if _, ok := confirms.get("slack"); !ok {
		t.Error("the pending row must survive a compensated write")
	}
	if strings.Contains(strings.ToLower(res.Content), "saved to shared") {
		t.Errorf("a compensated write must not imply success; got: %s", res.Content)
	}
}

// When the shared write path is NOT fully wired (no NED gate), a granted
// confirmation falls back to the slice-3 terminal (audit + delete + not-built),
// UNCHANGED — the NED gate is never run and nothing is persisted.
func TestRememberSharedNED_NotWiredFallsBackToSlice3Terminal(t *testing.T) {
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)
	// chatMemory + sharedNED nil → not fully wired.
	te := sharedExecutor(confirms, audit)

	content := "Bob owns the rollout"
	seedAck(confirms, content)
	res := te.remember(sharedCtx(), `{"content":"`+content+`","scope":"shared"}`, "proj-x")

	low := strings.ToLower(res.Content)
	if !strings.Contains(low, "authorized") || !strings.Contains(low, "not built") {
		t.Errorf("an unwired shared write must keep the slice-3 terminal (authorized + not built); got: %s", res.Content)
	}
	if audit.count() != 1 {
		t.Errorf("the slice-3 terminal writes the audit row; got %d", audit.count())
	}
}

// The token guardrail (review I3), from the dispatcher's vantage: the zero
// SharedWriteAuthorization is unusable, so even if a caller reached
// rememberSharedWrite with a forged zero token, it would refuse. The real
// guarantee is structural — the parameter type cannot be constructed granted
// outside package ned — this asserts the runtime backstop.
func TestRememberSharedWrite_ZeroTokenRefuses(t *testing.T) {
	w := &fakeChatMemoryWriter{}
	te := sharedNEDExecutor(newFakeConfirmRepo(nil), newFakeAuditRepo(nil), w, &fakeLinker{}, nil)
	rec := &persistence.ChatMemoryWriteConfirmation{Channel: "slack", SessionID: "sess"}

	res := te.rememberSharedWrite(context.Background(), rec, "slack", "sess", "proj-x", "x",
		time.Now(), ned.SharedWriteAuthorization{}, nil)

	if w.ingestCalls != 0 {
		t.Errorf("a zero (ungranted) token must not reach the pipeline; ingestCalls=%d", w.ingestCalls)
	}
	if strings.Contains(strings.ToLower(res.Content), "saved to shared") {
		t.Errorf("a zero token must not imply a save; got: %s", res.Content)
	}
}

// SetChatMemoryNED write-through regression guard (the 1234aea37 class): a
// setter that assigned only the agent field would leave the executor holding
// nil and the shared write path permanently reporting "not built".
func TestSetChatMemoryNED_WritesThroughToTheToolExecutor(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	a.SetChatMemoryNED(nedGate(&fakeNEDExtractor{}, &fakeNEDResolver{}))
	if a.toolExecutor.sharedNED == nil {
		t.Fatal("tool executor still holds nil — the shared write path would report not-built forever")
	}
	// Nil-safe on nil agent / nil executor.
	var nilA *Agent
	nilA.SetChatMemoryNED(nil)
	(&Agent{}).SetChatMemoryNED(nil)
}
