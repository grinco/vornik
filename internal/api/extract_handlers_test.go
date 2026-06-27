// Tests for the document-extraction HTTP surface. Each case
// exercises the handler directly (no real HTTP listener) so the
// route-glue + handler-body invariants are pinned without booting a
// full router.
package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/epub"
	"vornik.io/vornik/internal/persistence"
)

// stubArtifactRepo serves a single artifact for the handler to load.
// Implements only the ArtifactRepository methods Extract uses.
type stubArtifactRepo struct{ art *persistence.Artifact }

func (s *stubArtifactRepo) Create(context.Context, *persistence.Artifact) error { return nil }
func (s *stubArtifactRepo) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if s.art != nil && s.art.ID == id {
		return s.art, nil
	}
	return nil, nil
}
func (s *stubArtifactRepo) GetByHash(context.Context, string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepo) List(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepo) Delete(context.Context, string) error               { return nil }
func (s *stubArtifactRepo) DeleteByExecutionID(context.Context, string) error  { return nil }
func (s *stubArtifactRepo) UpdateTaskID(context.Context, string, string) error { return nil }

// stubExtractedDocsRepo captures Upsert calls and serves cached rows.
type stubExtractedDocsRepo struct {
	upserts []*persistence.ExtractedDocument
	cached  *persistence.ExtractedDocument
}

func (s *stubExtractedDocsRepo) Upsert(_ context.Context, d *persistence.ExtractedDocument) error {
	clone := *d
	s.upserts = append(s.upserts, &clone)
	return nil
}
func (s *stubExtractedDocsRepo) Get(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (s *stubExtractedDocsRepo) GetByArtifact(context.Context, string) (*persistence.ExtractedDocument, error) {
	return s.cached, nil
}
func (s *stubExtractedDocsRepo) ListByProject(context.Context, string, int) ([]*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (s *stubExtractedDocsRepo) Delete(context.Context, string) error { return nil }

// recordingIndexer captures the IngestExtractedSections payload so
// the test can assert on shape (one section per outline entry, etc).
type recordingIndexer struct {
	calls []indexCall
	err   error
}

type indexCall struct {
	projectID, taskID, artifactID, docID string
	sections                             []ExtractedSectionInput
}

func (r *recordingIndexer) IngestExtractedSections(
	_ context.Context,
	projectID, taskID, artifactID, docID string,
	sections []ExtractedSectionInput,
) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.calls = append(r.calls, indexCall{projectID, taskID, artifactID, docID, sections})
	total := 0
	for _, s := range sections {
		// Pretend each section produced one chunk so the response
		// shape carries a non-zero count for the happy-path assertion.
		_ = s
		total++
	}
	return total, nil
}

// buildExtractTestFixture writes a small EPUB to disk and returns
// (artifact, artifact-base-path, project-id, registry, runner,
// docs-repo, indexer). The fixture EPUB matches the one in the epub
// package's test — same shape, different temp path.
func buildExtractTestFixture(t *testing.T) (*persistence.Artifact, string, *stubExtractedDocsRepo, *recordingIndexer, *extractor.Registry, *extractor.Runner) {
	t.Helper()
	baseDir := t.TempDir()
	epubPath := filepath.Join(baseDir, "schema-coaching.epub")
	writeFixtureEPUB(t, epubPath)

	mime := "application/epub+zip"
	art := &persistence.Artifact{
		ID:          "art-1",
		ProjectID:   "test-project",
		Name:        "schema-coaching.epub",
		StoragePath: epubPath,
		MimeType:    &mime,
	}

	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), mime); err != nil {
		t.Fatalf("register epub: %v", err)
	}
	docs := &stubExtractedDocsRepo{}
	runner := &extractor.Runner{Repo: docs, BasePath: baseDir}
	indexer := &recordingIndexer{}
	return art, baseDir, docs, indexer, reg, runner
}

func writeFixtureEPUB(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create epub: %v", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	add("META-INF/container.xml", `<?xml version="1.0" encoding="UTF-8"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`)
	add("OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Test Book</dc:title>
    <dc:creator>Test Author</dc:creator>
  </metadata>
  <manifest>
    <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="c1"/>
  </spine>
</package>`)
	add("OEBPS/c1.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>
<h1>Chapter One</h1>
<p>This is a test chapter with content.</p>
</body></html>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// callExtract simulates a hit on
// POST /api/v1/projects/{projectId}/artifacts/{artifactId}/extract by
// constructing the request with the PathValue set the way the manual
// router does at routes.go.
func callExtract(s *Server, projectID, artifactID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+projectID+"/artifacts/"+artifactID+"/extract", nil)
	req.SetPathValue("projectId", projectID)
	req.SetPathValue("artifactId", artifactID)
	rec := httptest.NewRecorder()
	s.ExtractArtifact(rec, req)
	return rec
}

func TestExtractArtifact_HappyPath(t *testing.T) {
	art, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	artRepo := &stubArtifactRepo{art: art}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(artRepo),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)

	rec := callExtract(s, art.ProjectID, art.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}

	var resp ExtractArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExtractorName != epub.Name {
		t.Errorf("ExtractorName = %q; want %q", resp.ExtractorName, epub.Name)
	}
	if resp.Title != "Test Book" {
		t.Errorf("Title = %q; want \"Test Book\"", resp.Title)
	}
	if resp.SectionCount != 1 {
		t.Errorf("SectionCount = %d; want 1", resp.SectionCount)
	}
	if resp.ChunksIngested == 0 {
		t.Error("ChunksIngested = 0; expected the indexer to receive at least one section")
	}
	if len(docs.upserts) != 1 {
		t.Errorf("docs.upserts = %d; want 1", len(docs.upserts))
	}
	if len(indexer.calls) != 1 {
		t.Fatalf("indexer calls = %d; want 1", len(indexer.calls))
	}
	call := indexer.calls[0]
	if call.projectID != art.ProjectID {
		t.Errorf("indexer projectID = %q", call.projectID)
	}
	if call.docID == "" || call.artifactID != art.ID {
		t.Errorf("indexer ids mismatched: doc=%q artifact=%q", call.docID, call.artifactID)
	}
	if len(call.sections) != 1 {
		t.Errorf("indexer received %d sections; want 1", len(call.sections))
	}
	// Source-name should include the book title so downstream
	// citations read naturally — Title verbatim when section title
	// matches, "Title · Section" otherwise.
	if !strings.Contains(call.sections[0].SourceName, "Test Book") {
		t.Errorf("section source name should include title; got %q", call.sections[0].SourceName)
	}
}

func TestExtractArtifact_MissingPipeline_Returns503(t *testing.T) {
	s := NewServer(WithLogger(zerolog.Nop()), WithArtifactRepository(&stubArtifactRepo{}))
	rec := callExtract(s, "test-project", "art-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 NOT_CONFIGURED", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("body missing 'not configured': %s", rec.Body.String())
	}
}

func TestExtractArtifact_UnknownArtifact_Returns404(t *testing.T) {
	_, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{}), // empty — no artifact
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, "test-project", "missing-id")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestExtractArtifact_CrossProject_Returns404(t *testing.T) {
	// An artifact belonging to project A must NOT extract when the
	// caller is scoped to project B — match the existing artifact
	// read path's cross-project posture.
	art, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{art: art}),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, "other-project", art.ID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 for cross-project access", rec.Code)
	}
}

func TestExtractArtifact_UnsupportedMime_Returns415(t *testing.T) {
	_, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	mime := "application/x-no-extractor"
	art := &persistence.Artifact{
		ID:          "art-x",
		ProjectID:   "test-project",
		Name:        "data.dat",
		StoragePath: "/dev/null",
		MimeType:    &mime,
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{art: art}),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, art.ProjectID, art.ID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d; want 415", rec.Code)
	}
}

func TestExtractArtifact_CachedRow_SkipsReExtraction(t *testing.T) {
	art, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	// Pre-populate the cache so the handler returns without running.
	docs.cached = &persistence.ExtractedDocument{
		ID:               "extdoc_cached_1",
		ProjectID:        art.ProjectID,
		SourceArtifactID: art.ID,
		ExtractorName:    epub.Name,
		ExtractorVersion: epub.Version,
		MimeType:         "application/epub+zip",
		StoragePath:      "/var/cache/whatever",
		MetadataBlob:     []byte(`{"title":"Cached"}`),
		OutlineBlob:      []byte(`[]`),
		SectionCount:     7,
		TotalTextBytes:   12345,
		Status:           persistence.ExtractedDocumentStatusOK,
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{art: art}),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, art.ProjectID, art.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp ExtractArtifactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExtractedDocumentID != "extdoc_cached_1" {
		t.Errorf("response should reference the cached row; got %q", resp.ExtractedDocumentID)
	}
	if resp.Title != "Cached" {
		t.Errorf("Title should come from cached metadata; got %q", resp.Title)
	}
	if len(docs.upserts) != 0 {
		t.Errorf("cached path should NOT Upsert; got %d", len(docs.upserts))
	}
	if len(indexer.calls) != 0 {
		t.Errorf("cached path should NOT re-index; got %d calls", len(indexer.calls))
	}
}

func TestExtractArtifact_NoMimeNoExtension_Returns415(t *testing.T) {
	_, _, docs, indexer, reg, runner := buildExtractTestFixture(t)
	art := &persistence.Artifact{
		ID:          "art-noext",
		ProjectID:   "test-project",
		Name:        "noextension", // also no MimeType pointer
		StoragePath: "/dev/null",
	}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{art: art}),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, art.ProjectID, art.ID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d; want 415", rec.Code)
	}
}

func TestExtractArtifact_IndexerError_StillReturnsOK(t *testing.T) {
	// Extraction is durable — memory ingest is best-effort. A
	// failing indexer must NOT bubble up as 5xx.
	art, _, docs, _, reg, runner := buildExtractTestFixture(t)
	indexer := &recordingIndexer{err: errors.New("embed down")}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithArtifactRepository(&stubArtifactRepo{art: art}),
		WithExtractorPipeline(reg, runner, docs, indexer),
	)
	rec := callExtract(s, art.ProjectID, art.ID)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 (indexer error is best-effort)", rec.Code)
	}
}
