// Tests for the service-layer adapter that lifts an
// email.AttachmentAutoExtractor onto the document-extraction
// pipeline. Verifies MIME dispatch, cache-hit short-circuit, and
// best-effort memory-ingest posture.
package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/epub"
	"vornik.io/vornik/internal/persistence"
)

// stubExtractedDocsRepo captures Upsert + serves cached lookups.
// Trimmed to the methods emailAutoExtractor reaches for.
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

type stubArtifactOpener struct {
	byID map[string][]byte
}

func (s stubArtifactOpener) Open(_ context.Context, artifactID string) (io.ReadCloser, error) {
	body, ok := s.byID[artifactID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
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
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`)
	add("OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Auto-Extract Test</dc:title>
    <dc:creator>Service Test</dc:creator>
  </metadata>
  <manifest>
    <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="c1"/></spine>
</package>`)
	add("OEBPS/c1.xhtml", `<?xml version="1.0"?>
<html xmlns="http://www.w3.org/1999/xhtml"><body>
<h1>Chapter</h1><p>Body.</p></body></html>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

func fixtureEPUBBytes(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "book.epub")
	writeFixtureEPUB(t, path)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture epub: %v", err)
	}
	return body
}

// TestEmailAutoExtractor_HappyPath drives a real EPUB through the
// adapter and verifies the summary the email channel receives.
// Memory indexer is nil here — the adapter must still return the
// summary with ChunksIngested=0.
func TestEmailAutoExtractor_HappyPath(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "book.epub")
	writeFixtureEPUB(t, src)

	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), "application/epub+zip"); err != nil {
		t.Fatalf("register: %v", err)
	}
	docs := &stubExtractedDocsRepo{}
	runner := &extractor.Runner{Repo: docs, BasePath: base}
	adapter := newEmailAutoExtractor(reg, runner, docs, nil, nil, zerolog.Nop())

	got, err := adapter.AutoExtract(context.Background(), email.AutoExtractRequest{
		ProjectID:   "proj",
		ArtifactID:  "art-1",
		Name:        "book.epub",
		MimeType:    "application/epub+zip",
		StoragePath: src,
	})
	if err != nil {
		t.Fatalf("AutoExtract: %v", err)
	}
	if got == nil {
		t.Fatal("nil summary on successful extraction")
	}
	if got.Title != "Auto-Extract Test" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Author != "Service Test" {
		t.Errorf("Author = %q", got.Author)
	}
	if got.SectionCount != 1 {
		t.Errorf("SectionCount = %d", got.SectionCount)
	}
	if got.ChunksIngested != 0 {
		t.Errorf("ChunksIngested = %d; want 0 with nil indexer", got.ChunksIngested)
	}
	if len(docs.upserts) != 1 {
		t.Errorf("upserts = %d; want 1", len(docs.upserts))
	}
}

func TestEmailAutoExtractor_UsesArtifactOpenerWhenWired(t *testing.T) {
	base := t.TempDir()
	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), "application/epub+zip"); err != nil {
		t.Fatalf("register: %v", err)
	}
	docs := &stubExtractedDocsRepo{}
	runner := &extractor.Runner{Repo: docs, BasePath: base}
	opener := stubArtifactOpener{byID: map[string][]byte{"art-1": fixtureEPUBBytes(t)}}
	adapter := newEmailAutoExtractor(reg, runner, docs, nil, opener, zerolog.Nop())

	got, err := adapter.AutoExtract(context.Background(), email.AutoExtractRequest{
		ProjectID:   "proj",
		ArtifactID:  "art-1",
		Name:        "book.epub",
		MimeType:    "application/epub+zip",
		StoragePath: filepath.Join(base, "missing-local-shadow.epub"),
	})
	if err != nil {
		t.Fatalf("AutoExtract: %v", err)
	}
	if got == nil || got.Title != "Auto-Extract Test" {
		t.Fatalf("summary = %+v, want extraction from opener bytes", got)
	}
	if len(docs.upserts) != 1 {
		t.Fatalf("upserts = %d; want 1", len(docs.upserts))
	}
}

// TestEmailAutoExtractor_UnknownMime returns (nil, nil) so the
// channel treats it as a silent skip. No error, no extraction.
func TestEmailAutoExtractor_UnknownMime(t *testing.T) {
	reg := extractor.NewRegistry()
	docs := &stubExtractedDocsRepo{}
	runner := &extractor.Runner{Repo: docs, BasePath: t.TempDir()}
	adapter := newEmailAutoExtractor(reg, runner, docs, nil, nil, zerolog.Nop())

	got, err := adapter.AutoExtract(context.Background(), email.AutoExtractRequest{
		ProjectID: "proj", ArtifactID: "art", Name: "blob.bin", MimeType: "application/octet-stream",
	})
	if err != nil {
		t.Errorf("unknown MIME must NOT error; got %v", err)
	}
	if got != nil {
		t.Errorf("unknown MIME must return nil summary; got %+v", got)
	}
}

// TestEmailAutoExtractor_CacheHitSkipsReExtraction — re-emailing
// the same book shouldn't re-run the parser. The adapter checks
// GetByArtifact and short-circuits when the (extractor name,
// version) matches with status=OK.
func TestEmailAutoExtractor_CacheHitSkipsReExtraction(t *testing.T) {
	base := t.TempDir()
	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), "application/epub+zip"); err != nil {
		t.Fatalf("register: %v", err)
	}
	docs := &stubExtractedDocsRepo{
		cached: &persistence.ExtractedDocument{
			ID:               "extdoc_cached",
			SourceArtifactID: "art-1",
			ExtractorName:    epub.Name,
			ExtractorVersion: epub.Version,
			Status:           persistence.ExtractedDocumentStatusOK,
			SectionCount:     5,
			MetadataBlob:     []byte(`{"title":"Cached","author":"X"}`),
		},
	}
	runner := &extractor.Runner{Repo: docs, BasePath: base}
	adapter := newEmailAutoExtractor(reg, runner, docs, nil, nil, zerolog.Nop())

	got, err := adapter.AutoExtract(context.Background(), email.AutoExtractRequest{
		ProjectID: "proj", ArtifactID: "art-1", Name: "any.epub",
		MimeType: "application/epub+zip", StoragePath: "/dev/null",
	})
	if err != nil {
		t.Fatalf("AutoExtract: %v", err)
	}
	if got == nil || got.ExtractedDocumentID != "extdoc_cached" {
		t.Errorf("cache hit should return cached ID; got %+v", got)
	}
	if len(docs.upserts) != 0 {
		t.Errorf("cache hit must NOT re-Upsert; got %d", len(docs.upserts))
	}
}

// TestEmailAutoExtractor_NilDeps returns a clear error so the
// channel logs + degrades gracefully rather than nil-deref deep
// in the extraction stack.
func TestEmailAutoExtractor_NilDeps(t *testing.T) {
	adapter := &emailAutoExtractor{} // all nil
	_, err := adapter.AutoExtract(context.Background(), email.AutoExtractRequest{
		ProjectID: "p", ArtifactID: "a",
		MimeType: "application/epub+zip", StoragePath: "/dev/null",
	})
	if err == nil || !errors.Is(err, err) /* nil-check guard */ {
		t.Errorf("nil-deps adapter must surface an error; got %v", err)
	}
}
