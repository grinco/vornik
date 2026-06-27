// Tests for the REST CreateTask → input-artifacts auto-extraction
// pipeline. Each case pins a contract the scripted (CLI / API /
// CI runner) callers depend on: base64 decoding shape, snapshot
// idempotency, context merging, and the "no auto-extractor wired"
// degradation posture.
package api

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/epub"
	"vornik.io/vornik/internal/persistence"
)

// stubInputArtifactStore records StoreInput calls and writes the
// content to a controlled directory so the extractor pipeline
// can run against real bytes. Implements the api.InputArtifactStore
// interface — narrow on purpose so tests don't drag the full
// artifacts.Store into the test binary.
type stubInputArtifactStore struct {
	baseDir string
	calls   []string
	nextID  int
	failOn  string // when set, errors when name == failOn
	mimeFor map[string]string
}

func (s *stubInputArtifactStore) StoreInput(_ context.Context, projectID, name, sourcePath string) (*persistence.Artifact, error) {
	s.calls = append(s.calls, name)
	if s.failOn != "" && name == s.failOn {
		return nil, errors.New("snapshot failure")
	}
	s.nextID++
	id := persistence.GenerateID("artifact")
	// Copy bytes from sourcePath into a deterministic path under
	// baseDir so the extractor has a real file to read. Mirrors
	// the production Store's behaviour.
	destDir := filepath.Join(s.baseDir, projectID, "inputs", id)
	_ = os.MkdirAll(destDir, 0o700)
	dest := filepath.Join(destDir, name)
	data, _ := os.ReadFile(sourcePath)
	_ = os.WriteFile(dest, data, 0o600)
	mime := "application/octet-stream"
	if v, ok := s.mimeFor[name]; ok {
		mime = v
	}
	return &persistence.Artifact{
		ID:          id,
		ProjectID:   projectID,
		Name:        name,
		StoragePath: dest,
		MimeType:    &mime,
	}, nil
}

// fixtureEPUBBytes builds a small valid EPUB byte slice for the
// REST tests. Mirrors the epub package's test fixture; kept in-
// package because importing the test helper across packages
// drags assertions transitively.
func fixtureEPUBBytes(t *testing.T, title, author string) []byte {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "fixture.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	zw := zip.NewWriter(f)
	add := func(name, body string) {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`)
	add("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>`+title+`</dc:title>
    <dc:creator>`+author+`</dc:creator>
  </metadata>
  <manifest>
    <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="c1"/></spine>
</package>`)
	add("OEBPS/c1.xhtml", `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>
<h1>Chapter One</h1><p>This is the first chapter.</p>
</body></html>`)
	_ = zw.Close()
	_ = f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// fixtureServerWithExtractor builds an api.Server wired with the
// real EPUB extractor + a stub artifact store + a stub extracted-
// docs repo. Reuses the per-test repo fixtures from the
// document_tools tests so behaviour is consistent across the two
// layers.
func fixtureServerWithExtractor(t *testing.T) (*Server, *stubInputArtifactStore, *stubExtractedDocsRepo) {
	t.Helper()
	baseDir := t.TempDir()
	store := &stubInputArtifactStore{baseDir: baseDir, mimeFor: map[string]string{}}
	docs := &stubExtractedDocsRepo{}
	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), "application/epub+zip", "application/epub"); err != nil {
		t.Fatalf("register: %v", err)
	}
	runner := &extractor.Runner{Repo: docs, BasePath: baseDir}
	indexer := &recordingIndexer{}
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithInputArtifactStore(store),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	return srv, store, docs
}

// TestProcessInputArtifacts_HappyPath drives a real EPUB through
// the REST snapshot + extract chain and verifies the resulting
// inputArtifactResult slice carries everything the context merger
// needs.
func TestProcessInputArtifacts_HappyPath(t *testing.T) {
	srv, store, docs := fixtureServerWithExtractor(t)
	epubData := fixtureEPUBBytes(t, "REST Book", "REST Author")
	encoded := base64.StdEncoding.EncodeToString(epubData)

	store.mimeFor["book.epub"] = "application/epub+zip"
	inputs := []InputArtifact{{Name: "book.epub", Content: encoded}}

	results, err := srv.processInputArtifacts(context.Background(), "proj-rest", inputs)
	if err != nil {
		t.Fatalf("processInputArtifacts: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d; want 1", len(results))
	}
	r := results[0]
	if r.ArtifactID == "" || r.StoragePath == "" {
		t.Errorf("result missing fields: %+v", r)
	}
	if r.Extraction == nil {
		t.Fatal("Extraction nil; auto-extract should have fired")
	}
	if r.Extraction["title"] != "REST Book" {
		t.Errorf("title = %v", r.Extraction["title"])
	}
	if r.Extraction["author"] != "REST Author" {
		t.Errorf("author = %v", r.Extraction["author"])
	}
	if len(store.calls) != 1 {
		t.Errorf("StoreInput calls = %d; want 1", len(store.calls))
	}
	if len(docs.upserts) != 1 {
		t.Errorf("extracted_documents upserts = %d; want 1", len(docs.upserts))
	}
}

func TestProcessInputArtifacts_NoStoreWired_Errors(t *testing.T) {
	srv := NewServer(WithLogger(zerolog.Nop()))
	_, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "x.txt", Content: base64.StdEncoding.EncodeToString([]byte("hi"))}})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected not-configured error; got %v", err)
	}
}

func TestProcessInputArtifacts_BadBase64_Errors(t *testing.T) {
	srv, _, _ := fixtureServerWithExtractor(t)
	_, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "x.txt", Content: "!!!not-base64!!!"}})
	if err == nil || !strings.Contains(err.Error(), "base64") {
		t.Errorf("expected base64-decode error; got %v", err)
	}
}

func TestProcessInputArtifacts_EmptyName_Errors(t *testing.T) {
	srv, _, _ := fixtureServerWithExtractor(t)
	_, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "", Content: base64.StdEncoding.EncodeToString([]byte("hi"))}})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected name-required error; got %v", err)
	}
}

func TestProcessInputArtifacts_PathTraversal_Rejected(t *testing.T) {
	srv, _, _ := fixtureServerWithExtractor(t)
	_, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "../escape.txt", Content: base64.StdEncoding.EncodeToString([]byte("hi"))}})
	if err == nil {
		t.Fatal("expected path-traversal rejection")
	}
}

func TestProcessInputArtifacts_SnapshotFailure_PropagatesError(t *testing.T) {
	srv, store, _ := fixtureServerWithExtractor(t)
	store.failOn = "broken.epub"
	_, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "broken.epub", Content: base64.StdEncoding.EncodeToString([]byte("data"))}})
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Errorf("expected store error to propagate; got %v", err)
	}
}

func TestProcessInputArtifacts_UnsupportedMime_NoExtraction(t *testing.T) {
	// Random binary content with no extractor — snapshot succeeds,
	// extraction is silently skipped (no error). The task still
	// creates with the raw artifact reference; the worker can
	// decide what to do.
	srv, store, _ := fixtureServerWithExtractor(t)
	store.mimeFor["data.bin"] = "application/x-no-extractor"
	results, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "data.bin", Content: base64.StdEncoding.EncodeToString([]byte("opaque"))}})
	if err != nil {
		t.Fatalf("unsupported MIME must NOT fail the request; got %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d; want 1", len(results))
	}
	if results[0].Extraction != nil {
		t.Errorf("Extraction must be nil for unsupported MIME; got %+v", results[0].Extraction)
	}
}

func TestProcessInputArtifacts_GenericMime_FallsBackToFilename(t *testing.T) {
	// Channels that emit application/octet-stream for everything
	// must still trigger the right extractor via filename
	// extension lookup. Same fallback the /extract endpoint uses.
	srv, store, _ := fixtureServerWithExtractor(t)
	store.mimeFor["book.epub"] = "application/octet-stream"
	epubData := fixtureEPUBBytes(t, "Fallback Book", "Filename Author")
	results, err := srv.processInputArtifacts(context.Background(), "p",
		[]InputArtifact{{Name: "book.epub", Content: base64.StdEncoding.EncodeToString(epubData)}})
	if err != nil {
		t.Fatalf("processInputArtifacts: %v", err)
	}
	if results[0].Extraction == nil {
		t.Fatal("filename-fallback should have triggered EPUB extractor")
	}
	if results[0].Extraction["title"] != "Fallback Book" {
		t.Errorf("title = %v", results[0].Extraction["title"])
	}
}

func TestMergeInputsIntoContext_PreservesCallerKeys(t *testing.T) {
	// The caller may have set context.prompt + other fields.
	// Merging must add the three artifact fields without
	// clobbering anything else.
	raw := json.RawMessage(`{"prompt":"process this","custom":42}`)
	results := []inputArtifactResult{
		{StoragePath: "/var/lib/vornik/artifacts/p/inputs/art1/book.epub", ArtifactID: "art1",
			Extraction: map[string]any{"title": "Test", "section_count": 5}},
	}
	merged, err := mergeInputsIntoContext(raw, results)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var ctx map[string]any
	if err := json.Unmarshal(merged, &ctx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ctx["prompt"] != "process this" {
		t.Errorf("prompt key dropped: %v", ctx["prompt"])
	}
	if ctx["custom"].(float64) != 42 {
		t.Errorf("custom key dropped: %v", ctx["custom"])
	}
	files, _ := ctx["inputFiles"].([]any)
	if len(files) != 1 {
		t.Errorf("inputFiles len = %d", len(files))
	}
	ids, _ := ctx["inputArtifactIDs"].([]any)
	if len(ids) != 1 || ids[0] != "art1" {
		t.Errorf("inputArtifactIDs = %v", ids)
	}
	ex, _ := ctx["inputExtractions"].([]any)
	if len(ex) != 1 {
		t.Errorf("inputExtractions len = %d", len(ex))
	}
}

func TestMergeInputsIntoContext_EmptyResults_PassesThrough(t *testing.T) {
	raw := json.RawMessage(`{"prompt":"x"}`)
	got, err := mergeInputsIntoContext(raw, nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("merge with no results must pass context through; got %s", string(got))
	}
}

func TestMergeInputsIntoContext_EmptyContextStillWorks(t *testing.T) {
	// First-class CreateTask with no context — just inputArtifacts.
	results := []inputArtifactResult{
		{StoragePath: "/x/y", ArtifactID: "id1"},
	}
	merged, err := mergeInputsIntoContext(nil, results)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var ctx map[string]any
	if err := json.Unmarshal(merged, &ctx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if files, _ := ctx["inputFiles"].([]any); len(files) != 1 {
		t.Errorf("inputFiles missing in nil-context merge")
	}
}

func TestDecodeArtifactContent_AcceptsMultipleEncodings(t *testing.T) {
	original := []byte("hello, document pipeline")
	for name, encoded := range map[string]string{
		"std":     base64.StdEncoding.EncodeToString(original),
		"raw-std": base64.RawStdEncoding.EncodeToString(original),
		"url":     base64.URLEncoding.EncodeToString(original),
		"raw-url": base64.RawURLEncoding.EncodeToString(original),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeArtifactContent(encoded)
			if err != nil {
				t.Fatalf("%s decode: %v", name, err)
			}
			if string(got) != string(original) {
				t.Errorf("round-trip mismatch: %q vs %q", got, original)
			}
		})
	}
}

func TestDecodeArtifactContent_EmptyErrors(t *testing.T) {
	if _, err := decodeArtifactContent(""); err == nil {
		t.Error("empty content must error")
	}
}
