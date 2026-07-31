package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/memory"
)

// fakeChatMemoryWriter records IngestChatMemory / DeleteChatMemory calls and
// returns a scripted result, so the personal-write path (slice 5) can be
// exercised without a real pipeline + DB.
type fakeChatMemoryWriter struct {
	result      memory.ChatMemoryIngestResult
	ingestErr   error
	ingestCalls int
	deleted     []string // artifact ids passed to DeleteChatMemory
}

func (f *fakeChatMemoryWriter) IngestChatMemory(_ context.Context, _, _, _, _ string) (memory.ChatMemoryIngestResult, error) {
	f.ingestCalls++
	return f.result, f.ingestErr
}

func (f *fakeChatMemoryWriter) DeleteChatMemory(_ context.Context, _, artifactID string) error {
	f.deleted = append(f.deleted, artifactID)
	return nil
}

// fakeLinker records data-subject linkage and can fail AddLink to drive the
// compensation path (D4.1a).
type fakeLinker struct {
	existingSubject string
	created         bool
	identifiers     []datasubject.Identifier
	links           []datasubject.Link
	addLinkErr      error
}

func (f *fakeLinker) FindSubjectByIdentifier(_ context.Context, _, _ string) (string, error) {
	return f.existingSubject, nil
}
func (f *fakeLinker) CreateSubject(_ context.Context, _ datasubject.Subject) error {
	f.created = true
	return nil
}
func (f *fakeLinker) AddIdentifier(_ context.Context, _ string, id datasubject.Identifier) error {
	f.identifiers = append(f.identifiers, id)
	return nil
}
func (f *fakeLinker) AddLink(_ context.Context, _ string, l datasubject.Link) error {
	if f.addLinkErr != nil {
		return f.addLinkErr
	}
	f.links = append(f.links, l)
	return nil
}

func personalExecutor(w ChatMemoryWriter, linker DataSubjectLinker) *ToolExecutor {
	return &ToolExecutor{
		memoryWrite:  &stubMemoryWriteGate{allow: map[string]bool{testMemChannel + "|" + testMemSession: true}},
		chatMemory:   w,
		dataSubjects: linker,
	}
}

// TestSetChatMemoryWriter_WritesThroughToTheToolExecutor — the same regression
// guard as SetMemoryWriteGate: NewAgent builds a.toolExecutor once, so a setter
// that assigns only the agent field leaves the executor holding nil and the
// personal write path permanently dark (the 1234aea37 class).
func TestSetChatMemoryWriter_WritesThroughToTheToolExecutor(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	a.SetChatMemoryWriter(&fakeChatMemoryWriter{}, &fakeLinker{})
	if a.toolExecutor.chatMemory == nil || a.toolExecutor.dataSubjects == nil {
		t.Fatal("tool executor still holds nil — the personal write path would report not-built forever")
	}
	// Nil-safe on nil agent / nil executor.
	var nilA *Agent
	nilA.SetChatMemoryWriter(nil, nil)
	(&Agent{}).SetChatMemoryWriter(nil, nil)
}

// TestRememberPersonal_PersistsAndLinks — the slice-5 happy path: a granted
// channel's personal remember writes a chat_memory chunk and links it to the
// operator's own data subject (design D4.1), then reports success truthfully.
func TestRememberPersonal_PersistsAndLinks(t *testing.T) {
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_1",
		ChunkIDs:   []string{"chunk-a", "chunk-b"},
		Stats:      memory.IngestStats{Admitted: 1},
	}}
	linker := &fakeLinker{}
	te := personalExecutor(w, linker)

	res := te.remember(sharedCtx(), `{"content":"I prefer short answers"}`, "proj-x")

	if w.ingestCalls != 1 {
		t.Fatalf("IngestChatMemory calls = %d, want 1", w.ingestCalls)
	}
	low := strings.ToLower(res.Content)
	if !strings.Contains(low, "saved") {
		t.Errorf("must report the save truthfully; got: %s", res.Content)
	}
	if len(w.deleted) != 0 {
		t.Errorf("no compensation delete expected on success; got %v", w.deleted)
	}
	// One link per chunk, targeting project_memory_chunks, at the operator-self
	// confidence/exclusivity.
	if len(linker.links) != 2 {
		t.Fatalf("links = %d, want 2 (one per chunk)", len(linker.links))
	}
	for _, l := range linker.links {
		if l.Table != datasubject.TableProjectMemoryChunks {
			t.Errorf("link table = %q, want project_memory_chunks", l.Table)
		}
		if l.Source != datasubject.SourceOperatorAsserted || l.Confidence != datasubject.ConfidenceProbable {
			t.Errorf("link source/confidence = %s/%s, want operator_asserted/probable", l.Source, l.Confidence)
		}
		if l.ProjectID != "proj-x" {
			t.Errorf("link project = %q, want proj-x", l.ProjectID)
		}
	}
	if !linker.created || len(linker.identifiers) != 1 {
		t.Errorf("a new subject + operator identifier must be created; created=%v ids=%d", linker.created, len(linker.identifiers))
	}
}

// TestRememberPersonal_CompensatesOnLinkFailure — D4.1a: if the post-insert
// data-subject link fails, the chunk must be deleted (not left unlinked), and
// the tool must not imply the memory was kept.
func TestRememberPersonal_CompensatesOnLinkFailure(t *testing.T) {
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_9",
		ChunkIDs:   []string{"chunk-a"},
		Stats:      memory.IngestStats{Admitted: 1},
	}}
	linker := &fakeLinker{addLinkErr: errors.New("db down")}
	te := personalExecutor(w, linker)

	res := te.remember(sharedCtx(), `{"content":"a fact worth keeping"}`, "proj-x")

	if len(w.deleted) != 1 || w.deleted[0] != "chatmem_9" {
		t.Fatalf("compensation must delete the artifact; deleted=%v", w.deleted)
	}
	low := strings.ToLower(res.Content)
	for _, forbidden := range []string{"saved", "kept it"} {
		if strings.Contains(low, forbidden) {
			t.Errorf("a compensated write must not imply success (%q): %s", forbidden, res.Content)
		}
	}
}

// TestRememberPersonal_NotWiredReportsNotBuilt — with no pipeline wired the
// personal path reports the slice-3 "not built" shape, byte-for-byte, so a
// deployment without the write path behaves exactly as before.
func TestRememberPersonal_NotWiredReportsNotBuilt(t *testing.T) {
	te := &ToolExecutor{memoryWrite: &stubMemoryWriteGate{allow: map[string]bool{testMemChannel + "|" + testMemSession: true}}}
	res := te.remember(sharedCtx(), `{"content":"x that is long enough"}`, "proj-x")
	if res != rememberSaveNotBuilt(memoryScopePersonal) {
		t.Errorf("unwired personal path must report not-built; got: %s", res.Content)
	}
}

// TestRememberPersonal_NoOperatorIdentityRefuses — a personal note has to
// belong to someone; with no resolvable operator id the tool refuses and
// nothing is written (§5.6.5).
func TestRememberPersonal_NoOperatorIdentityRefuses(t *testing.T) {
	w := &fakeChatMemoryWriter{}
	te := personalExecutor(w, &fakeLinker{})
	// Call site set, but no operator id on the context.
	ctx := WithCallSiteForTest(context.Background(), testMemChannel, testMemSession)
	res := te.remember(ctx, `{"content":"a personal note"}`, "proj-x")
	if w.ingestCalls != 0 {
		t.Errorf("must not write without an operator identity; ingestCalls=%d", w.ingestCalls)
	}
	if !strings.Contains(strings.ToLower(res.Content), "who is speaking") {
		t.Errorf("must refuse citing unknown speaker; got: %s", res.Content)
	}
}

// TestRememberPersonal_GatedOutNotKept — when the pipeline admits nothing
// (dedup / too short / secret-dump), the tool says so rather than implying a
// save, and does not attempt a link.
func TestRememberPersonal_GatedOutNotKept(t *testing.T) {
	w := &fakeChatMemoryWriter{result: memory.ChatMemoryIngestResult{
		ArtifactID: "chatmem_2",
		Stats:      memory.IngestStats{Admitted: 0},
	}}
	linker := &fakeLinker{}
	te := personalExecutor(w, linker)

	res := te.remember(sharedCtx(), `{"content":"dup"}`, "proj-x")
	if len(linker.links) != 0 {
		t.Errorf("no link when nothing was admitted; links=%d", len(linker.links))
	}
	if strings.Contains(strings.ToLower(res.Content), "saved") {
		t.Errorf("a gated-out deposit must not imply a save; got: %s", res.Content)
	}
}
