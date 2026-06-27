package rag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
)

// ---- repoScopeFromPayload coverage ----

// TestRepoScopeFromPayloadCov covers the empty-payload and
// malformed-JSON branches that the B-18 happy-path tests don't.
func TestRepoScopeFromPayloadCov(t *testing.T) {
	if got := repoScopeFromPayload(nil); got != "" {
		t.Errorf("empty payload → empty scope, got %q", got)
	}
	if got := repoScopeFromPayload([]byte("not json")); got != "" {
		t.Errorf("malformed payload → empty scope, got %q", got)
	}
	if got := repoScopeFromPayload([]byte(`{"context":{"repo_scope":"  "}}`)); got != "" {
		t.Errorf("whitespace scope → empty, got %q", got)
	}
}

// ---- ExtractHandler error branches ----

// TestExtractHandlerCov_MissingDeps covers the nil-dependency guard.
func TestExtractHandlerCov_MissingDeps(t *testing.T) {
	h := NewExtractHandler(nil, nil, nil)
	_, err := h.Execute(context.Background(), executor.SystemStepInput{Task: &persistence.Task{ID: "t"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required dependencies")
}

// TestExtractHandlerCov_NilTask covers the nil-task guard.
func TestExtractHandlerCov_NilTask(t *testing.T) {
	reg := extractor.NewRegistry()
	runner := &extractor.Runner{Repo: &fakeExtractedDocRepo{}, BasePath: t.TempDir()}
	h := NewExtractHandler(reg, runner, &fakeArtifactRepo{})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{Task: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task is nil")
}

// TestExtractHandlerCov_ArtifactGetError covers the artifact-lookup
// error branch.
func TestExtractHandlerCov_ArtifactGetError(t *testing.T) {
	reg := extractor.NewRegistry()
	runner := &extractor.Runner{Repo: &fakeExtractedDocRepo{}, BasePath: t.TempDir()}
	// Artifact repo with no rows → Get returns "not found".
	h := NewExtractHandler(reg, runner, &fakeArtifactRepo{byID: map[string]*persistence.Artifact{}})
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"inputArtifactIDs": []string{"missing"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: &persistence.Task{ID: "t", ProjectID: "p", Payload: payload},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load artifact")
}

// TestExtractHandlerCov_NilMimeFilenameFallback covers the empty-MIME
// → MimeFromFilename fallback branch, plus the runner.Run error
// branch (the fake extractor errors).
func TestExtractHandlerCov_NilMimeFilenameFallbackAndRunError(t *testing.T) {
	fake := &fakeExtractor{name: "f", version: "1", err: errors.New("extract boom")}
	reg := extractor.NewRegistry()
	// Register for the MIME that MimeFromFilename("note.md") resolves to.
	mime := extractor.MimeFromFilename("note.md")
	require.NoError(t, reg.Register(fake, mime))
	runner := &extractor.Runner{Repo: &fakeExtractedDocRepo{}, BasePath: t.TempDir()}

	// Artifact with a NIL MimeType pointer so the filename fallback runs.
	art := &persistence.Artifact{ID: "a1", ProjectID: "p", Name: "note.md", StoragePath: "/tmp/note.md"}
	h := NewExtractHandler(reg, runner, &fakeArtifactRepo{byID: map[string]*persistence.Artifact{"a1": art}})
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"inputArtifactIDs": []string{"a1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: &persistence.Task{ID: "t", ProjectID: "p", Payload: payload},
	})
	// The MIME fallback resolved + dispatched, then the extractor errored.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "extract a1")
}

// ---- IndexHandler error branches ----

// TestIndexHandlerCov_MissingDeps covers the nil-dependency guard.
func TestIndexHandlerCov_MissingDeps(t *testing.T) {
	h := NewIndexHandler(nil, nil)
	_, err := h.Execute(context.Background(), executor.SystemStepInput{Task: &persistence.Task{ID: "t"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required dependencies")
}

// TestIndexHandlerCov_NilTask covers the nil-task guard.
func TestIndexHandlerCov_NilTask(t *testing.T) {
	h := NewIndexHandler(&fakeExtractedDocRepo{}, &fakeIndexer{})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{Task: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "task is nil")
}

// TestIndexHandlerCov_BadPrevResult covers the parse-previous-result
// branch.
func TestIndexHandlerCov_BadPrevResult(t *testing.T) {
	h := NewIndexHandler(&fakeExtractedDocRepo{}, &fakeIndexer{})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: json.RawMessage(`{not json`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse previous result")
}

// TestIndexHandlerCov_DocGetError covers the docRepo.Get error
// branch.
func TestIndexHandlerCov_DocGetError(t *testing.T) {
	// Empty repo → Get returns "not found".
	h := NewIndexHandler(&fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{}}, &fakeIndexer{})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "missing"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load extracted doc")
}

// TestIndexHandlerCov_EmptyStoragePath covers the empty-storage_path
// guard.
func TestIndexHandlerCov_EmptyStoragePath(t *testing.T) {
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", StoragePath: ""}, // empty
	}}
	h := NewIndexHandler(docRepo, &fakeIndexer{})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty storage_path")
}

// TestIndexHandlerCov_BadOutlineBlob covers the outline-parse error
// branch.
func TestIndexHandlerCov_BadOutlineBlob(t *testing.T) {
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", StoragePath: t.TempDir(), OutlineBlob: []byte(`{bad`)},
	}}
	h := NewIndexHandler(docRepo, &fakeIndexer{})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse outline")
}

// TestIndexHandlerCov_EmptyOutlineSkips covers the empty-outline
// silent-skip branch: a doc with no sections is skipped, yielding a
// zero-chunk success.
func TestIndexHandlerCov_EmptyOutlineSkips(t *testing.T) {
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", StoragePath: t.TempDir(), OutlineBlob: []byte(`[]`)},
	}}
	idx := &fakeIndexer{}
	h := NewIndexHandler(docRepo, idx)
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.NoError(t, err)
	if len(idx.calls) != 0 {
		t.Error("empty outline should skip ingestion entirely")
	}
	var out struct{ Chunks, Documents int }
	require.NoError(t, json.Unmarshal(res.Result, &out))
	assert.Equal(t, 0, out.Chunks)
}

// TestIndexHandlerCov_SectionReadError covers the section-file read
// error: the outline references a section .md file that isn't on disk.
func TestIndexHandlerCov_SectionReadError(t *testing.T) {
	baseDir := t.TempDir()
	docDir := filepath.Join(baseDir, "d1")
	require.NoError(t, os.MkdirAll(filepath.Join(docDir, "sections"), 0o700))
	// Outline references a section whose .md file we deliberately omit.
	outline, _ := json.Marshal([]extractor.OutlineEntry{{SectionID: "ghost", Title: "Ghost", TextBytes: 1}})
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", StoragePath: docDir, OutlineBlob: outline},
	}}
	h := NewIndexHandler(docRepo, &fakeIndexer{})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read section")
}

// TestIndexHandlerCov_IngestError covers the IngestExtractedSections
// error branch.
func TestIndexHandlerCov_IngestError(t *testing.T) {
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "d1", []extractor.Section{{SectionID: "01", Title: "X", Content: "hi"}})
	outline, _ := json.Marshal([]extractor.OutlineEntry{{SectionID: "01", Title: "X", TextBytes: 2}})
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", SourceArtifactID: "a", StoragePath: storagePath, OutlineBlob: outline},
	}}
	idx := &fakeIndexer{err: errors.New("ingest boom")}
	h := NewIndexHandler(docRepo, idx)
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p"},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ingest d1")
}

// indexCov_patchErrPatcher errors on PatchScopeByArtifact.
type indexCov_patchErrPatcher struct {
	fakeIndexer
}

func (p *indexCov_patchErrPatcher) PatchScopeByArtifact(_ context.Context, _, _, _ string) error {
	return errors.New("patch boom")
}

// TestIndexHandlerCov_PatchScopeError covers the patch-scope error
// branch (after a successful ingest, the scope tag fails).
func TestIndexHandlerCov_PatchScopeError(t *testing.T) {
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "d1", []extractor.Section{{SectionID: "01", Title: "X", Content: "hi"}})
	outline, _ := json.Marshal([]extractor.OutlineEntry{{SectionID: "01", Title: "X", TextBytes: 2}})
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", SourceArtifactID: "a", StoragePath: storagePath, OutlineBlob: outline},
	}}
	h := NewIndexHandler(docRepo, &indexCov_patchErrPatcher{})
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"repo_scope": "github.com/x/y"}})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{{"artifact_id": "a", "extracted_document_id": "d1"}}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p", Payload: payload},
		PrevResult: prev,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patch scope")
}

// TestIndexHandlerCov_PatchScopeDedupAndTransientGet covers the
// per-source-artifact dedup branch (two extracted docs sharing one
// source artifact → one patch call) AND tolerates a doc that was
// loaded earlier. Uses the recording fakeScopePatcher from rag_test.go.
func TestIndexHandlerCov_PatchScopeDedup(t *testing.T) {
	baseDir := t.TempDir()
	mk := func(id string) string {
		return writeExtractedDoc(t, baseDir, id, []extractor.Section{{SectionID: "01", Title: "X", Content: id}})
	}
	outline, _ := json.Marshal([]extractor.OutlineEntry{{SectionID: "01", Title: "X", TextBytes: 1}})
	// Two extracted docs sharing the SAME SourceArtifactID.
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{
		"d1": {ID: "d1", ProjectID: "p", SourceArtifactID: "shared", StoragePath: mk("d1"), OutlineBlob: outline},
		"d2": {ID: "d2", ProjectID: "p", SourceArtifactID: "shared", StoragePath: mk("d2"), OutlineBlob: outline},
	}}
	patcher := &fakeScopePatcher{}
	h := NewIndexHandler(docRepo, patcher)
	payload, _ := json.Marshal(map[string]any{"context": map[string]any{"repo_scope": "github.com/x/y"}})
	prev, _ := json.Marshal(map[string]any{"extracted": []map[string]any{
		{"artifact_id": "shared", "extracted_document_id": "d1"},
		{"artifact_id": "shared", "extracted_document_id": "d2"},
	}})
	_, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t", ProjectID: "p", Payload: payload},
		PrevResult: prev,
	})
	require.NoError(t, err)
	if len(patcher.patches) != 1 {
		t.Errorf("two docs sharing one source artifact should patch once, got %d", len(patcher.patches))
	}
}
