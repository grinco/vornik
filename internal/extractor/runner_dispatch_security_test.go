// Runner-dispatch security characterization — batch-3 ingress/
// untrusted-input. The epub package's own tests
// (TestExtract_DecompressionRatio_Cap, TestExtract_XMLExternalEntity_
// NotResolved, …) prove the zip-bomb / XXE guards fire when the EPUB
// extractor is invoked DIRECTLY via epub.New().Extract(...). Those
// never touch the registered-dispatch seam the daemon actually uses:
//
//	Registry.For(mime) -> Runner.Run(...) -> ext.Extract(...)
//
// This file closes that gap. It wires the real epub extractor into a
// real Registry exactly as internal/service/container_extractor.go
// does, resolves the extractor by MIME, and drives a malicious EPUB
// through Runner.Run. The load-bearing extra assertions over the
// direct-call tests are end-to-end: the guard rejects the input
// BEFORE the Runner mints a doc ID, creates a storage dir, or Upserts
// a row — so a bomb leaves NO persisted document and NO orphan dir.
//
// Lives in package extractor_test (not the internal extractor test
// package) on purpose: epub imports extractor, so an internal test
// importing epub would form an import cycle. The external test
// package is compiled separately and may depend on epub.
package extractor_test

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/epub"
	"vornik.io/vornik/internal/persistence"
)

// dispatchFakeRepo records Upserts so the test can assert the bomb
// never produced a persisted extracted_documents row. Mirrors the
// repo contract Runner.Run depends on.
type dispatchFakeRepo struct {
	mu      sync.Mutex
	upserts []*persistence.ExtractedDocument
}

func (f *dispatchFakeRepo) Upsert(_ context.Context, d *persistence.ExtractedDocument) error {
	clone := *d
	f.mu.Lock()
	f.upserts = append(f.upserts, &clone)
	f.mu.Unlock()
	return nil
}
func (*dispatchFakeRepo) Get(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*dispatchFakeRepo) GetByArtifact(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*dispatchFakeRepo) ListByProject(context.Context, string, int) ([]*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*dispatchFakeRepo) Delete(context.Context, string) error { return nil }

func (f *dispatchFakeRepo) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

// registeredEpubRunner builds the Registry + Runner the same way the
// daemon's container wiring does: epub registered against both the
// canonical and the +zip-stripped MIME, a fresh BasePath, and a repo
// fake. Returns the registry, runner, repo, and the BasePath so the
// caller can assert no storage dir leaked.
func registeredEpubRunner(t *testing.T) (*extractor.Registry, *extractor.Runner, *dispatchFakeRepo, string) {
	t.Helper()
	reg := extractor.NewRegistry()
	if err := reg.Register(epub.New(), "application/epub+zip", "application/epub"); err != nil {
		t.Fatalf("register epub: %v", err)
	}
	repo := &dispatchFakeRepo{}
	base := t.TempDir()
	r := &extractor.Runner{Repo: repo, BasePath: base}
	return reg, r, repo, base
}

// extractedDirEntries lists the per-doc directories the Runner would
// have created under <base>/<project>/extracted/. Empty == no storage
// leaked. A non-existent dir is treated as empty (the Runner only
// MkdirAll's it on a successful extraction).
func extractedDirEntries(t *testing.T, base, project string) []string {
	t.Helper()
	dir := filepath.Join(base, project, "extracted")
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read extracted dir: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// writeRatioBombEPUB writes a kilobyte-scale archive whose single
// entry declares a 64 MiB uncompressed payload — a >200:1 ratio that
// trips the epub extractor's decompression-ratio pre-check. (No
// META-INF/container.xml: the ratio guard runs first, so the archive
// is rejected before container resolution is even attempted.)
func writeRatioBombEPUB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ratio-bomb.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	w, err := zw.Create("big.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	// 64 MiB of a single byte compresses to a few KiB → ratio far
	// above the 200:1 cap, but well under the 256 MiB absolute cap so
	// it is the RATIO branch that rejects it.
	if _, err := w.Write([]byte(strings.Repeat("A", 64<<20))); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

// writeXXEEpub writes an EPUB whose container.xml carries an external
// SYSTEM entity pointing at a local secret file. If Go's XML decoder
// resolved it (it must not), the secret bytes would surface in the
// rootfile path — i.e. an XXE file-disclosure.
func writeXXEEpub(t *testing.T) (epubPath, secretPath string) {
	t.Helper()
	secretPath = filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP-SECRET-DISPATCH-CANARY"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := t.TempDir()
	epubPath = filepath.Join(dir, "xxe.epub")
	f, err := os.Create(epubPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	w, err := zw.Create("META-INF/container.xml")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(`<?xml version="1.0"?>
<!DOCTYPE container [ <!ENTITY xxe SYSTEM "file://` + secretPath + `"> ]>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="&xxe;" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return epubPath, secretPath
}

// TestRunnerDispatch_ZipBomb_RejectedNoPersist — a decompression-ratio
// bomb routed through the REGISTERED dispatch path (Registry.For ->
// Runner.Run -> epub.Extract) is rejected with the extractor's
// decompression-bomb error, and — the part the direct-call epub test
// cannot assert — the Runner Upserts NO row and leaves NO storage dir.
func TestRunnerDispatch_ZipBomb_RejectedNoPersist(t *testing.T) {
	reg, runner, repo, base := registeredEpubRunner(t)
	const project = "assistant"

	// Dispatch by MIME exactly as the daemon does. Use the +zip-
	// stripped form to also exercise the alias registration.
	ext, err := reg.For("application/epub")
	if err != nil {
		t.Fatalf("registry did not resolve epub for application/epub: %v", err)
	}
	if ext.Name() != epub.Name {
		t.Fatalf("registry resolved %q; want the epub extractor %q", ext.Name(), epub.Name)
	}

	bomb := writeRatioBombEPUB(t)
	row, err := runner.Run(context.Background(), project, "art-bomb", ext, extractor.Source{
		FilePath:     bomb,
		MimeType:     "application/epub",
		OriginalName: "ratio-bomb.epub",
	})
	if err == nil {
		t.Fatal("expected zip-bomb to be rejected through Runner.Run; got nil error")
	}
	if row != nil {
		t.Errorf("expected nil row on rejection; got %+v", row)
	}
	// Error must originate from the extractor's bomb guard, wrapped by
	// the Runner. "decompression bomb" / "ratio" come from the epub
	// guard; "runner: extract" confirms it flowed through dispatch.
	if !strings.Contains(err.Error(), "runner: extract") {
		t.Errorf("error not wrapped by Runner dispatch: %v", err)
	}
	if !strings.Contains(err.Error(), "ratio") && !strings.Contains(err.Error(), "decompression bomb") {
		t.Errorf("expected decompression-bomb/ratio guard error; got %v", err)
	}

	// End-to-end invariant: rejection happens upstream of persistence.
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("a rejected bomb produced %d Upsert(s); want 0 (no extracted_documents row)", n)
	}
	if dirs := extractedDirEntries(t, base, project); len(dirs) != 0 {
		t.Errorf("a rejected bomb left storage dirs %v; want none (no orphan)", dirs)
	}
}

// TestRunnerDispatch_XXE_NotResolvedNoPersist — an external-entity
// (XXE) EPUB routed through the registered dispatch path is rejected,
// the secret is never disclosed in the error, and no row/dir is
// persisted. Guards the stdlib-default-holds invariant at the seam the
// daemon actually invokes.
func TestRunnerDispatch_XXE_NotResolvedNoPersist(t *testing.T) {
	reg, runner, repo, base := registeredEpubRunner(t)
	const project = "assistant"

	ext, err := reg.For("application/epub+zip")
	if err != nil {
		t.Fatalf("registry did not resolve epub for application/epub+zip: %v", err)
	}

	xxe, _ := writeXXEEpub(t)
	row, err := runner.Run(context.Background(), project, "art-xxe", ext, extractor.Source{
		FilePath:     xxe,
		MimeType:     "application/epub+zip",
		OriginalName: "xxe.epub",
	})
	if err == nil {
		t.Fatal("expected XXE EPUB to fail through Runner.Run (entity not resolved); got nil error")
	}
	if row != nil {
		t.Errorf("expected nil row on rejection; got %+v", row)
	}
	if strings.Contains(err.Error(), "TOP-SECRET-DISPATCH-CANARY") {
		t.Fatalf("external entity resolved through dispatch — XXE file-disclosure: %v", err)
	}
	if !strings.Contains(err.Error(), "runner: extract") {
		t.Errorf("error not wrapped by Runner dispatch: %v", err)
	}
	if n := repo.upsertCount(); n != 0 {
		t.Errorf("a rejected XXE EPUB produced %d Upsert(s); want 0", n)
	}
	if dirs := extractedDirEntries(t, base, project); len(dirs) != 0 {
		t.Errorf("a rejected XXE EPUB left storage dirs %v; want none", dirs)
	}
}
