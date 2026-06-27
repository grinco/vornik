// Package rag holds the deterministic "system" step handlers used
// by the document-ingest workflow (B-7). RED tests live here in
// the same package so the interfaces under test are visible
// without re-exporting them.
//
// These tests pin the handler contract Phase 2 must implement:
//   - rag.NewExtractHandler returns a SystemHandler whose Execute
//     reads inputArtifactIDs from the task payload, runs the
//     MIME-matched extractor, and emits extracted_document_id(s)
//     in result.json.
//   - rag.NewIndexHandler returns a SystemHandler whose Execute
//     reads the prior step's extracted_document_id(s), loads the
//     sections, and calls Indexer.IngestExtractedSections.
//
// Both handlers must work without an LLM call so the workflow's
// "zero-token deterministic ingest" promise holds.
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
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ---- Test doubles --------------------------------------------------

// fakeExtractor satisfies extractor.Extractor with operator-scripted
// behaviour. The registry dispatch path looks the extractor up by
// MIME and the runner calls Extract — both surfaces are exercised
// through this stub.
type fakeExtractor struct {
	name    string
	version string
	result  extractor.Result
	err     error
	calls   int
}

func (f *fakeExtractor) Name() string    { return f.name }
func (f *fakeExtractor) Version() string { return f.version }
func (f *fakeExtractor) Extract(_ context.Context, _ extractor.Source) (extractor.Result, error) {
	f.calls++
	if f.err != nil {
		return extractor.Result{}, f.err
	}
	return f.result, nil
}

// fakeArtifactRepo returns canned artifact rows by ID. Handlers
// resolve the source MIME type from the artifact row, so the test
// pre-populates rows it expects the handler to fetch.
type fakeArtifactRepo struct {
	byID map[string]*persistence.Artifact
}

func (f *fakeArtifactRepo) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if a, ok := f.byID[id]; ok {
		return a, nil
	}
	return nil, errors.New("not found")
}

// fakeExtractedDocRepo captures Upsert calls so the index test
// can assert the chunk-count path was reached. Implements the
// full persistence.ExtractedDocumentRepository interface so we
// can pass it to both the runner (Upsert) and the handler (Get).
type fakeExtractedDocRepo struct {
	byID    map[string]*persistence.ExtractedDocument
	upserts []*persistence.ExtractedDocument
}

func (f *fakeExtractedDocRepo) Upsert(_ context.Context, row *persistence.ExtractedDocument) error {
	if f.byID == nil {
		f.byID = map[string]*persistence.ExtractedDocument{}
	}
	f.byID[row.ID] = row
	f.upserts = append(f.upserts, row)
	return nil
}

func (f *fakeExtractedDocRepo) Get(_ context.Context, id string) (*persistence.ExtractedDocument, error) {
	if r, ok := f.byID[id]; ok {
		return r, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeExtractedDocRepo) GetByArtifact(_ context.Context, _ string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (f *fakeExtractedDocRepo) ListByProject(_ context.Context, _ string, _ int) ([]*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (f *fakeExtractedDocRepo) Delete(_ context.Context, _ string) error { return nil }

// fakeIndexer records IngestExtractedSections calls so the index
// handler test can pin (a) it was called and (b) the section
// content reached it.
type fakeIndexer struct {
	calls []indexerCall
	err   error
}

type indexerCall struct {
	ProjectID  string
	TaskID     string
	ArtifactID string
	DocID      string
	Sections   []memory.ExtractedSection
}

// newTestArtifact builds an Artifact with the mime+path the test
// needs without struggling against the *string Mime field.
func newTestArtifact(id, projectID, name, mime, storagePath string) *persistence.Artifact {
	m := mime
	return &persistence.Artifact{
		ID:          id,
		ProjectID:   projectID,
		Name:        name,
		StoragePath: storagePath,
		MimeType:    &m,
	}
}

func (f *fakeIndexer) IngestExtractedSections(_ context.Context, projectID, taskID, artifactID, docID string, sections []memory.ExtractedSection) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.calls = append(f.calls, indexerCall{projectID, taskID, artifactID, docID, sections})
	count := 0
	for _, s := range sections {
		if s.Content != "" {
			count++
		}
	}
	return count, nil
}

// PatchScopeByArtifact is a no-op on the base fakeIndexer; the
// happy-path index tests don't assert on scope. fakeScopePatcher
// embeds fakeIndexer and overrides PatchScopeByArtifact when the
// B-18 tests need to record calls.
func (f *fakeIndexer) PatchScopeByArtifact(_ context.Context, _, _, _ string) error {
	return nil
}

// ---- Extract handler tests ----------------------------------------

// TestExtractHandler_RunsRegisteredExtractor — happy path: the
// handler resolves an input artifact's MIME, dispatches to the
// matching extractor, persists the extracted document, and emits
// the extracted_document_id in result.json.
func TestExtractHandler_RunsRegisteredExtractor(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Wire registry + runner with a markdown-claiming fake extractor.
	fake := &fakeExtractor{
		name:    "fake-text",
		version: "1.0.0",
		result: extractor.Result{
			Metadata: extractor.Metadata{Title: "Test note"},
			Outline: []extractor.OutlineEntry{
				{SectionID: "01-body", Title: "Body", TextBytes: 4},
			},
			Sections: []extractor.Section{
				{SectionID: "01-body", Title: "Body", Content: "hello"},
			},
		},
	}
	reg := extractor.NewRegistry()
	require.NoError(t, reg.Register(fake, "text/markdown"))

	docRepo := &fakeExtractedDocRepo{}
	runner := &extractor.Runner{Repo: docRepo, BasePath: tmpDir}

	artRepo := &fakeArtifactRepo{byID: map[string]*persistence.Artifact{
		"art_1": newTestArtifact("art_1", "p1", "test.md", "text/markdown", "/tmp/test.md"),
	}}

	h := NewExtractHandler(reg, runner, artRepo)

	payload := map[string]any{
		"context": map[string]any{
			"inputArtifactIDs": []string{"art_1"},
		},
	}
	payloadBytes, _ := json.Marshal(payload)
	in := executor.SystemStepInput{
		Task: &persistence.Task{
			ID:        "task_1",
			ProjectID: "p1",
			Payload:   payloadBytes,
		},
		Execution: &persistence.Execution{ID: "exec_1"},
		StepID:    "extract",
		Step:      &registry.WorkflowStep{Type: "system", Handler: "rag.extract"},
	}

	res, err := h.Execute(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.calls, "extractor.Extract should be invoked once")
	assert.Len(t, docRepo.upserts, 1, "extracted_documents row should be persisted")

	// Result must surface extracted_document_id so the next step
	// (rag.index) can find it.
	var resultObj struct {
		Extracted []struct {
			ArtifactID          string `json:"artifact_id"`
			ExtractedDocumentID string `json:"extracted_document_id"`
		} `json:"extracted"`
	}
	require.NoError(t, json.Unmarshal(res.Result, &resultObj))
	require.Len(t, resultObj.Extracted, 1)
	assert.Equal(t, "art_1", resultObj.Extracted[0].ArtifactID)
	assert.NotEmpty(t, resultObj.Extracted[0].ExtractedDocumentID)
}

// TestExtractHandler_NoExtractorRegistered — unsupported MIME
// surfaces a clear error so the workflow's on_fail branch can
// route operator-readable status. Bypassing this would silently
// produce zero chunks downstream.
func TestExtractHandler_NoExtractorRegistered(t *testing.T) {
	reg := extractor.NewRegistry()
	runner := &extractor.Runner{Repo: &fakeExtractedDocRepo{}, BasePath: t.TempDir()}
	artRepo := &fakeArtifactRepo{byID: map[string]*persistence.Artifact{
		"art_1": newTestArtifact("art_1", "p1", "x.bin", "application/x-unknown", "/tmp/x"),
	}}

	h := NewExtractHandler(reg, runner, artRepo)
	payload, _ := json.Marshal(map[string]any{
		"context": map[string]any{"inputArtifactIDs": []string{"art_1"}},
	})
	in := executor.SystemStepInput{
		Task:      &persistence.Task{ID: "t1", ProjectID: "p1", Payload: payload},
		Execution: &persistence.Execution{ID: "e1"},
		Step:      &registry.WorkflowStep{Type: "system", Handler: "rag.extract"},
	}

	_, err := h.Execute(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "application/x-unknown")
}

// TestExtractHandler_NoInputArtifacts — a workflow run with no
// input artifacts produces a clear error rather than a silent
// zero-document success. Same defensive shape as the existing
// rag-ingester role's hard-rule list.
func TestExtractHandler_NoInputArtifacts(t *testing.T) {
	reg := extractor.NewRegistry()
	runner := &extractor.Runner{Repo: &fakeExtractedDocRepo{}, BasePath: t.TempDir()}

	h := NewExtractHandler(reg, runner, &fakeArtifactRepo{})
	in := executor.SystemStepInput{
		Task:      &persistence.Task{ID: "t1", ProjectID: "p1", Payload: []byte("{}")},
		Execution: &persistence.Execution{ID: "e1"},
		Step:      &registry.WorkflowStep{Type: "system", Handler: "rag.extract"},
	}

	_, err := h.Execute(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "input")
}

// TestExtractHandler_Name — every SystemHandler returns the name
// the executor's handler registry keys on.
func TestExtractHandler_Name(t *testing.T) {
	h := NewExtractHandler(nil, nil, nil)
	assert.Equal(t, "rag.extract", h.Name())
}

// ---- Index handler tests ------------------------------------------

// writeExtractedDoc seeds a fake on-disk layout matching what
// extractor.Runner produces — metadata.json, outline.json, plus
// sections/*.md — so the index handler reads what it expects.
func writeExtractedDoc(t *testing.T, baseDir, docID string, sections []extractor.Section) string {
	t.Helper()
	dir := filepath.Join(baseDir, docID)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sections"), 0o700))
	outline := make([]extractor.OutlineEntry, 0, len(sections))
	for _, s := range sections {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "sections", s.SectionID+".md"),
			[]byte(s.Content), 0o600))
		outline = append(outline, extractor.OutlineEntry{
			SectionID: s.SectionID, Title: s.Title, TextBytes: len(s.Content),
		})
	}
	outlineBytes, _ := json.Marshal(outline)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "outline.json"),
		outlineBytes, 0o600))
	metaBytes, _ := json.Marshal(extractor.Metadata{Title: "Test"})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.json"),
		metaBytes, 0o600))
	return dir
}

// TestIndexHandler_IngestsExtractedSections — happy path: the
// handler reads the previous step's extracted_document_id, loads
// each section, and calls IngestExtractedSections with the
// expected shape.
func TestIndexHandler_IngestsExtractedSections(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "extdoc_1", []extractor.Section{
		{SectionID: "01-intro", Title: "Intro", Content: "Hello world."},
		{SectionID: "02-body", Title: "Body", Content: "More content."},
	})

	outlineBytes, _ := json.Marshal([]extractor.OutlineEntry{
		{SectionID: "01-intro", Title: "Intro", TextBytes: 12},
		{SectionID: "02-body", Title: "Body", TextBytes: 13},
	})
	metaBytes, _ := json.Marshal(extractor.Metadata{Title: "Test note"})

	docRepo := &fakeExtractedDocRepo{
		byID: map[string]*persistence.ExtractedDocument{
			"extdoc_1": {
				ID:               "extdoc_1",
				ProjectID:        "p1",
				SourceArtifactID: "art_1",
				StoragePath:      storagePath,
				OutlineBlob:      outlineBytes,
				MetadataBlob:     metaBytes,
				SectionCount:     2,
			},
		},
	}
	idx := &fakeIndexer{}

	h := NewIndexHandler(docRepo, idx)

	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_1", "extracted_document_id": "extdoc_1"},
		},
	})
	in := executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1"},
		Execution:  &persistence.Execution{ID: "exec_1"},
		StepID:     "index",
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	}

	res, err := h.Execute(ctx, in)
	require.NoError(t, err)
	require.Len(t, idx.calls, 1, "IngestExtractedSections must be called once")
	call := idx.calls[0]
	assert.Equal(t, "p1", call.ProjectID)
	assert.Equal(t, "task_1", call.TaskID)
	assert.Equal(t, "art_1", call.ArtifactID)
	assert.Equal(t, "extdoc_1", call.DocID)
	assert.Len(t, call.Sections, 2, "every outlined section must be ingested")

	// Result must carry the chunk count so observers (the workflow
	// finalizer + the operator-facing log line) can report progress.
	var resultObj struct {
		Chunks    int `json:"chunks"`
		Documents int `json:"documents"`
	}
	require.NoError(t, json.Unmarshal(res.Result, &resultObj))
	assert.Equal(t, 2, resultObj.Chunks)
	assert.Equal(t, 1, resultObj.Documents)
}

// TestIndexHandler_NoExtractedDocuments — running rag.index when
// the prior step left no extracted documents is a workflow-author
// error. Surface it, don't silently no-op.
func TestIndexHandler_NoExtractedDocuments(t *testing.T) {
	h := NewIndexHandler(&fakeExtractedDocRepo{}, &fakeIndexer{})
	in := executor.SystemStepInput{
		Task:       &persistence.Task{ID: "t1", ProjectID: "p1"},
		Execution:  &persistence.Execution{ID: "e1"},
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: json.RawMessage(`{"extracted":[]}`),
	}
	_, err := h.Execute(context.Background(), in)
	require.Error(t, err)
}

// TestIndexHandler_Name — handler-registry key.
func TestIndexHandler_Name(t *testing.T) {
	h := NewIndexHandler(nil, nil)
	assert.Equal(t, "rag.index", h.Name())
}

// B-18 (repo_scope on extracted chunks) -----------------------------

// patchCall captures one PatchScopeByArtifact invocation.
type patchCall struct {
	ProjectID  string
	ArtifactID string
	RepoScope  string
}

// fakeScopePatcher records PatchScopeByArtifact calls so the
// B-18 tests can assert (a) the call happened and (b) the
// arguments matched the per-source-artifact + payload-scope
// contract.
type fakeScopePatcher struct {
	fakeIndexer
	patches []patchCall
}

func (f *fakeScopePatcher) PatchScopeByArtifact(_ context.Context, projectID, artifactID, repoScope string) error {
	f.patches = append(f.patches, patchCall{projectID, artifactID, repoScope})
	return nil
}

// TestIndexHandler_StampsRepoScopeFromContextPayload — pins B-18:
// when the task payload carries context.repo_scope, rag.index
// must call PatchScopeByArtifact for each ingested source
// artifact so chunks land tagged. Without this fix the chunks
// land NULL-scoped and strict-scope recalls miss them.
func TestIndexHandler_StampsRepoScopeFromContextPayload(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "extdoc_1", []extractor.Section{
		{SectionID: "01-body", Title: "Body", Content: "hello"},
	})
	outlineBytes, _ := json.Marshal([]extractor.OutlineEntry{
		{SectionID: "01-body", Title: "Body", TextBytes: 5},
	})
	docRepo := &fakeExtractedDocRepo{
		byID: map[string]*persistence.ExtractedDocument{
			"extdoc_1": {
				ID:               "extdoc_1",
				ProjectID:        "p1",
				SourceArtifactID: "art_src_1",
				StoragePath:      storagePath,
				OutlineBlob:      outlineBytes,
				SectionCount:     1,
			},
		},
	}
	patcher := &fakeScopePatcher{}
	h := NewIndexHandler(docRepo, patcher)

	payload, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"repo_scope": "github.com/owner/repo",
		},
	})
	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_src_1", "extracted_document_id": "extdoc_1"},
		},
	})
	in := executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1", Payload: payload},
		Execution:  &persistence.Execution{ID: "exec_1"},
		StepID:     "index",
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	}
	_, err := h.Execute(ctx, in)
	require.NoError(t, err)
	require.Len(t, patcher.patches, 1)
	assert.Equal(t, "p1", patcher.patches[0].ProjectID)
	assert.Equal(t, "art_src_1", patcher.patches[0].ArtifactID,
		"PatchScope must target the SOURCE artifact_id — that's what IngestExtractedSections stamps on each chunk")
	assert.Equal(t, "github.com/owner/repo", patcher.patches[0].RepoScope)
}

// TestIndexHandler_StampsRepoScopeFromTopLevelPayload — B-18
// supports both payload shapes (legacy unnested `repo_scope`
// alongside the canonical `context.repo_scope`), same contract
// as the executor's extractRepoScopeFromPayload helper.
func TestIndexHandler_StampsRepoScopeFromTopLevelPayload(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "extdoc_2", []extractor.Section{
		{SectionID: "01-body", Title: "Body", Content: "x"},
	})
	outlineBytes, _ := json.Marshal([]extractor.OutlineEntry{
		{SectionID: "01-body", Title: "Body", TextBytes: 1},
	})
	docRepo := &fakeExtractedDocRepo{
		byID: map[string]*persistence.ExtractedDocument{
			"extdoc_2": {
				ID: "extdoc_2", ProjectID: "p1",
				SourceArtifactID: "art_src_2",
				StoragePath:      storagePath, OutlineBlob: outlineBytes,
			},
		},
	}
	patcher := &fakeScopePatcher{}
	h := NewIndexHandler(docRepo, patcher)

	payload, _ := json.Marshal(map[string]any{
		"repo_scope": "github.com/owner/legacy-shape",
	})
	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_src_2", "extracted_document_id": "extdoc_2"},
		},
	})
	_, err := h.Execute(ctx, executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1", Payload: payload},
		Execution:  &persistence.Execution{ID: "exec_1"},
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	})
	require.NoError(t, err)
	require.Len(t, patcher.patches, 1)
	assert.Equal(t, "github.com/owner/legacy-shape", patcher.patches[0].RepoScope)
}

// TestIndexHandler_NoScopePatchWhenAbsent — without a repo_scope
// on the payload, rag.index must NOT call PatchScopeByArtifact.
// Otherwise an empty-string scope would wipe any pre-existing
// scope tag on the artifact's chunks.
func TestIndexHandler_NoScopePatchWhenAbsent(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	storagePath := writeExtractedDoc(t, baseDir, "extdoc_3", []extractor.Section{
		{SectionID: "01-body", Title: "Body", Content: "x"},
	})
	outlineBytes, _ := json.Marshal([]extractor.OutlineEntry{
		{SectionID: "01-body", Title: "Body", TextBytes: 1},
	})
	docRepo := &fakeExtractedDocRepo{
		byID: map[string]*persistence.ExtractedDocument{
			"extdoc_3": {
				ID: "extdoc_3", ProjectID: "p1",
				SourceArtifactID: "art_src_3",
				StoragePath:      storagePath, OutlineBlob: outlineBytes,
			},
		},
	}
	patcher := &fakeScopePatcher{}
	h := NewIndexHandler(docRepo, patcher)

	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_src_3", "extracted_document_id": "extdoc_3"},
		},
	})
	_, err := h.Execute(ctx, executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1", Payload: []byte(`{}`)},
		Execution:  &persistence.Execution{ID: "exec_1"},
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	})
	require.NoError(t, err)
	assert.Len(t, patcher.patches, 0,
		"empty payload scope must not trigger a PatchScopeByArtifact (would wipe pre-existing tags)")
}

// TestIndexHandler_PatchesEverySourceArtifact — when one task
// ingests N source files, every source artifact's chunks must
// get scope-tagged. The patcher contract is per-artifact, not
// per-extracted-document.
func TestIndexHandler_PatchesEverySourceArtifact(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	mkDoc := func(id, srcID string) {
		storagePath := writeExtractedDoc(t, baseDir, id, []extractor.Section{
			{SectionID: "01-body", Title: "Body", Content: id},
		})
		outline, _ := json.Marshal([]extractor.OutlineEntry{
			{SectionID: "01-body", Title: "Body", TextBytes: len(id)},
		})
		_ = storagePath
		// Loaded into docRepo below.
		_ = outline
	}
	mkDoc("extdoc_a", "art_a")
	mkDoc("extdoc_b", "art_b")

	// Build the doc repo with both rows after the test dirs exist.
	docRepo := &fakeExtractedDocRepo{byID: map[string]*persistence.ExtractedDocument{}}
	for _, pair := range []struct{ docID, srcID string }{
		{"extdoc_a", "art_a"}, {"extdoc_b", "art_b"},
	} {
		outline, _ := json.Marshal([]extractor.OutlineEntry{
			{SectionID: "01-body", Title: "Body", TextBytes: 8},
		})
		docRepo.byID[pair.docID] = &persistence.ExtractedDocument{
			ID:               pair.docID,
			ProjectID:        "p1",
			SourceArtifactID: pair.srcID,
			StoragePath:      baseDir + "/" + pair.docID,
			OutlineBlob:      outline,
		}
	}

	patcher := &fakeScopePatcher{}
	h := NewIndexHandler(docRepo, patcher)
	payload, _ := json.Marshal(map[string]any{
		"context": map[string]any{"repo_scope": "github.com/x/y"},
	})
	prev, _ := json.Marshal(map[string]any{
		"extracted": []map[string]any{
			{"artifact_id": "art_a", "extracted_document_id": "extdoc_a"},
			{"artifact_id": "art_b", "extracted_document_id": "extdoc_b"},
		},
	})
	_, err := h.Execute(ctx, executor.SystemStepInput{
		Task:       &persistence.Task{ID: "task_1", ProjectID: "p1", Payload: payload},
		Execution:  &persistence.Execution{ID: "exec_1"},
		Step:       &registry.WorkflowStep{Type: "system", Handler: "rag.index"},
		PrevResult: prev,
	})
	require.NoError(t, err)
	require.Len(t, patcher.patches, 2)
	artifacts := []string{patcher.patches[0].ArtifactID, patcher.patches[1].ArtifactID}
	assert.ElementsMatch(t, []string{"art_a", "art_b"}, artifacts)
	for _, p := range patcher.patches {
		assert.Equal(t, "github.com/x/y", p.RepoScope)
	}
}
