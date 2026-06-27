// Tests for the EPUB extractor. The fixture builder synthesises a
// minimal but spec-conformant .epub on disk so the test exercises
// the real zip / OPF / XHTML parsing path — not a mock.
package epub

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
)

// buildFixtureEPUB writes a minimal EPUB to a temp file. Two
// chapters, basic metadata, OPF spine in reading order. Returns the
// path so tests can hand it to Extractor.Extract.
func buildFixtureEPUB(t *testing.T, opts fixtureOpts) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.epub")
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
    <dc:title>`+opts.title+`</dc:title>
    <dc:creator opf:role="aut">`+opts.author+`</dc:creator>
    <dc:publisher>`+opts.publisher+`</dc:publisher>
    <dc:date>`+opts.date+`</dc:date>
    <dc:language>en</dc:language>
    <dc:identifier scheme="ISBN">`+opts.isbn+`</dc:identifier>
  </metadata>
  <manifest>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
  </spine>
</package>`)

	add("OEBPS/ch1.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>Chapter One</title></head>
<body>
  <h1>The Beginning</h1>
  <p>This is the first paragraph of chapter one. It introduces the world.</p>
  <p>Second paragraph. Sets the mood.</p>
  <script>alert('xss')</script>
</body>
</html>`)

	add("OEBPS/ch2.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>Chapter Two</title></head>
<body>
  <h2>The Middle</h2>
  <p>Things get complicated.</p>
  <blockquote>A quoted insight from a wise character.</blockquote>
  <ul><li>Point A</li><li>Point B</li></ul>
</body>
</html>`)

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return path
}

type fixtureOpts struct {
	title, author, publisher, date, isbn string
}

func defaultFixture() fixtureOpts {
	return fixtureOpts{
		title:     "Schema Coaching",
		author:    "Iain McCormick",
		publisher: "Routledge",
		date:      "2017",
		isbn:      "9781138067592",
	}
}

func TestExtract_HappyPath(t *testing.T) {
	path := buildFixtureEPUB(t, defaultFixture())
	ext := New()
	res, err := ext.Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "application/epub+zip",
		OriginalName: "schema-coaching.epub",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Metadata round-trip.
	if res.Metadata.Title != "Schema Coaching" {
		t.Errorf("Title = %q", res.Metadata.Title)
	}
	if res.Metadata.Author != "Iain McCormick" {
		t.Errorf("Author = %q", res.Metadata.Author)
	}
	if res.Metadata.Publisher != "Routledge" {
		t.Errorf("Publisher = %q", res.Metadata.Publisher)
	}
	if res.Metadata.PublicationDate != "2017" {
		t.Errorf("PublicationDate = %q", res.Metadata.PublicationDate)
	}
	if res.Metadata.ISBN != "9781138067592" {
		t.Errorf("ISBN = %q", res.Metadata.ISBN)
	}
	if res.Metadata.Language != "en" {
		t.Errorf("Language = %q", res.Metadata.Language)
	}

	// Spine order preserved.
	if len(res.Sections) != 2 {
		t.Fatalf("sections = %d; want 2", len(res.Sections))
	}
	if !strings.HasPrefix(res.Sections[0].SectionID, "001-") {
		t.Errorf("section[0].ID = %q; want 001- prefix for spine order", res.Sections[0].SectionID)
	}
	if !strings.HasPrefix(res.Sections[1].SectionID, "002-") {
		t.Errorf("section[1].ID = %q; want 002- prefix", res.Sections[1].SectionID)
	}

	// Outline matches sections 1:1.
	if len(res.Outline) != len(res.Sections) {
		t.Fatalf("outline (%d) != sections (%d)", len(res.Outline), len(res.Sections))
	}
	for i := range res.Outline {
		if res.Outline[i].SectionID != res.Sections[i].SectionID {
			t.Errorf("outline[%d].SectionID = %q; sections[%d].SectionID = %q",
				i, res.Outline[i].SectionID, i, res.Sections[i].SectionID)
		}
		if res.Outline[i].TextBytes != len(res.Sections[i].Content) {
			t.Errorf("outline[%d].TextBytes = %d; section content len = %d",
				i, res.Outline[i].TextBytes, len(res.Sections[i].Content))
		}
	}

	// First chapter content sanity check — heading + both paragraphs
	// present, the <script> body is NOT.
	body0 := res.Sections[0].Content
	if !strings.Contains(body0, "# The Beginning") {
		t.Errorf("section[0] missing heading; got:\n%s", body0)
	}
	if !strings.Contains(body0, "first paragraph") {
		t.Errorf("section[0] missing paragraph; got:\n%s", body0)
	}
	if strings.Contains(body0, "alert(") || strings.Contains(body0, "xss") {
		t.Errorf("section[0] leaked <script> body:\n%s", body0)
	}

	// Second chapter — block elements rendered as markdown-ish.
	body1 := res.Sections[1].Content
	if !strings.Contains(body1, "## The Middle") {
		t.Errorf("section[1] missing h2; got:\n%s", body1)
	}
	if !strings.Contains(body1, "> A quoted insight") {
		t.Errorf("section[1] blockquote not rendered as > prefix; got:\n%s", body1)
	}
	if !strings.Contains(body1, "- Point A") || !strings.Contains(body1, "- Point B") {
		t.Errorf("section[1] list items not rendered as -; got:\n%s", body1)
	}

	// Section title falls back to the first heading.
	if res.Sections[0].Title != "The Beginning" {
		t.Errorf("section[0].Title = %q; want \"The Beginning\"", res.Sections[0].Title)
	}
}

func TestExtract_FallbackToOriginalName_WhenNoTitle(t *testing.T) {
	opts := defaultFixture()
	opts.title = ""
	path := buildFixtureEPUB(t, opts)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "application/epub+zip",
		OriginalName: "my-secret-manuscript.epub",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Title != "my-secret-manuscript" {
		t.Errorf("Title fallback = %q; want \"my-secret-manuscript\" (extension stripped)", res.Metadata.Title)
	}
}

func TestExtract_MissingContainerXML(t *testing.T) {
	// A zip without META-INF/container.xml is not a valid EPUB —
	// fail loudly rather than reporting a successful zero-section
	// extraction.
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.epub")
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	_, _ = zw.Create("OEBPS/content.opf") // present but unreachable
	_ = zw.Close()
	_ = f.Close()

	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path, MimeType: "application/epub+zip"})
	if err == nil || !strings.Contains(err.Error(), "container.xml") {
		t.Errorf("expected container.xml error; got %v", err)
	}
}

func TestExtract_ContextCancellation(t *testing.T) {
	path := buildFixtureEPUB(t, defaultFixture())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — spine walk should bail on first iteration
	_, err := New().Extract(ctx, extractor.Source{FilePath: path, MimeType: "application/epub+zip"})
	if !errors.Is(err, context.Canceled) {
		// The error wraps via fmt.Errorf("%w", ...) so an unwrap check
		// confirms the cancellation was honoured.
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("expected cancelled context to surface; got %v", err)
		}
	}
}

func TestExtract_EmptyFilePath(t *testing.T) {
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: ""})
	if err == nil {
		t.Error("expected error for empty FilePath")
	}
}

// buildBombEPUB writes an otherwise-valid EPUB whose spine XHTML
// files together expand far past the cumulative uncompressed budget.
// The per-file cap alone (maxFileBytes) would let N near-cap files
// through; the cumulative cap is what stops the aggregate blow-up.
// Each chapter is highly compressible (repeated bytes) so the
// on-disk archive stays tiny while the uncompressed total is huge —
// the classic decompression-bomb shape.
func buildBombEPUB(t *testing.T, chapters int, bytesPerChapter int) string {
	t.Helper()
	dir := t.TempDir()
	pathStr := filepath.Join(dir, "bomb.epub")
	f, err := os.Create(pathStr)
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

	var manifest, spine strings.Builder
	for i := 0; i < chapters; i++ {
		id := fmt.Sprintf("ch%d", i)
		fmt.Fprintf(&manifest, `<item id="%s" href="%s.xhtml" media-type="application/xhtml+xml"/>`+"\n", id, id)
		fmt.Fprintf(&spine, `<itemref idref="%s"/>`+"\n", id)
	}
	add("OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Bomb</dc:title></metadata>
  <manifest>`+manifest.String()+`</manifest>
  <spine>`+spine.String()+`</spine>
</package>`)

	// Highly compressible filler so the zip stays small on disk.
	filler := strings.Repeat("A", bytesPerChapter)
	for i := 0; i < chapters; i++ {
		add(fmt.Sprintf("OEBPS/ch%d.xhtml", i),
			`<html xmlns="http://www.w3.org/1999/xhtml"><body><p>`+filler+`</p></body></html>`)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return pathStr
}

// TestExtract_DecompressionBomb_CumulativeCap — batch-3
// ingress/untrusted-input: document-extraction hardening (a).
// Many chapters, each just under the per-file cap, together blow
// past the cumulative uncompressed budget. Pre-fix this extracts
// (per-file LimitReader passes each one); post-fix the cumulative
// budget rejects it before the heap is exhausted.
func TestExtract_DecompressionBomb_CumulativeCap(t *testing.T) {
	// 40 chapters × 8 MiB = 320 MiB uncompressed, well past the
	// 256 MiB cumulative cap, but each chapter is under maxFileBytes
	// (10 MiB) so the per-file guard does not catch it.
	path := buildBombEPUB(t, 40, 8<<20)
	_, err := New().Extract(context.Background(), extractor.Source{
		FilePath: path, MimeType: "application/epub+zip",
	})
	if err == nil {
		t.Fatal("expected decompression-bomb rejection; got nil error")
	}
	if !strings.Contains(err.Error(), "uncompressed") {
		t.Errorf("expected cumulative-uncompressed cap error; got %v", err)
	}
}

// TestExtract_DecompressionRatio_Cap — batch-3 ingress/untrusted-
// input: document-extraction hardening (a). A tiny archive that
// declares a vastly larger uncompressed total trips the ratio cap
// up front, before any entry is read into memory.
func TestExtract_DecompressionRatio_Cap(t *testing.T) {
	// 80 chapters × 4 MiB = 320 MiB uncompressed from a kilobyte-
	// scale archive — both over the absolute 256 MiB cap and at a
	// ratio far above 200:1; either branch rejects it.
	path := buildBombEPUB(t, 80, 4<<20)
	_, err := New().Extract(context.Background(), extractor.Source{
		FilePath: path, MimeType: "application/epub+zip",
	})
	if err == nil {
		t.Fatal("expected decompression-ratio rejection; got nil error")
	}
	if !strings.Contains(err.Error(), "ratio") && !strings.Contains(err.Error(), "uncompressed") {
		t.Errorf("expected ratio/uncompressed cap error; got %v", err)
	}
}

// TestExtract_XMLBillionLaughs_Rejected — batch-3 ingress/untrusted-
// input: document-extraction hardening (c). An EPUB whose
// container.xml carries a billion-laughs entity-expansion payload
// must be rejected (or bounded), never expanded. Go's encoding/xml
// does not expand custom DOCTYPE entities, so this asserts the
// invariant stays true (regression guard against anyone wiring a
// custom Decoder.Entity map).
func TestExtract_XMLBillionLaughs_Rejected(t *testing.T) {
	dir := t.TempDir()
	pathStr := filepath.Join(dir, "laughs.epub")
	f, _ := os.Create(pathStr)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("META-INF/container.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?>
<!DOCTYPE container [
 <!ENTITY lol "lol">
 <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
 <!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">
 <!ENTITY lol4 "&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;&lol3;">
 <!ENTITY lol5 "&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;&lol4;">
]>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="&lol5;" media-type="application/oebps-package+xml"/></rootfiles>
</container>`))
	_ = zw.Close()
	_ = f.Close()

	_, err := New().Extract(context.Background(), extractor.Source{
		FilePath: pathStr, MimeType: "application/epub+zip",
	})
	if err == nil {
		t.Fatal("expected billion-laughs payload to be rejected, not expanded")
	}
	// The error must come from the XML layer (entity not expanded),
	// not from a multi-gigabyte allocation succeeding.
	if !strings.Contains(err.Error(), "container.xml") && !strings.Contains(err.Error(), "entity") {
		t.Errorf("expected XML/entity rejection; got %v", err)
	}
}

// TestExtract_XMLExternalEntity_NotResolved — batch-3 ingress/
// untrusted-input: document-extraction hardening (c). XXE: an
// external SYSTEM entity must not be resolved (no file read, no
// network). Asserts the stdlib default holds.
func TestExtract_XMLExternalEntity_NotResolved(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-XXE-CANARY"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := t.TempDir()
	pathStr := filepath.Join(dir, "xxe.epub")
	f, _ := os.Create(pathStr)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("META-INF/container.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?>
<!DOCTYPE container [ <!ENTITY xxe SYSTEM "file://` + secret + `"> ]>
<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="&xxe;" media-type="application/oebps-package+xml"/></rootfiles>
</container>`))
	_ = zw.Close()
	_ = f.Close()

	_, err := New().Extract(context.Background(), extractor.Source{
		FilePath: pathStr, MimeType: "application/epub+zip",
	})
	if err == nil {
		t.Fatal("expected external-entity EPUB to fail (entity not resolved)")
	}
	if strings.Contains(err.Error(), "TOP-SECRET-XXE-CANARY") {
		t.Fatalf("external entity was resolved — XXE leak: %v", err)
	}
}

// TestReadBudget_Charge unit-tests the cumulative budget directly:
// it bounds the AGGREGATE uncompressed bytes even when individual
// entries (and lying zip metadata) slip past the per-file and
// ratio pre-checks. batch-3 hardening (a).
func TestReadBudget_Charge(t *testing.T) {
	b := newReadBudget(100)
	if err := b.charge(60); err != nil {
		t.Fatalf("first charge under budget: %v", err)
	}
	if err := b.charge(40); err != nil {
		t.Fatalf("second charge exactly at budget should pass: %v", err)
	}
	if err := b.charge(1); err == nil {
		t.Fatal("charge over budget must error")
	} else if !strings.Contains(err.Error(), "uncompressed") {
		t.Errorf("budget error = %v; want uncompressed-cap message", err)
	}
}

// TestCheckDecompressionRatio_HighRatioUnderAbsoluteCap exercises
// the ratio branch specifically: a declared total UNDER the absolute
// cap but with a tiny on-disk archive, so the ratio (not the
// absolute cap) is what trips. batch-3 hardening (a).
func TestCheckDecompressionRatio_HighRatioUnderAbsoluteCap(t *testing.T) {
	dir := t.TempDir()
	pathStr := filepath.Join(dir, "ratio.epub")
	f, err := os.Create(pathStr)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	// One highly-compressible 64 MiB entry (< 256 MiB absolute cap)
	// from a kilobyte-scale archive → ratio far above 200:1.
	w, _ := zw.Create("big.txt")
	if _, err := w.Write([]byte(strings.Repeat("A", 64<<20))); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = zw.Close()
	_ = f.Close()

	zr, err := zip.OpenReader(pathStr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = zr.Close() }()
	err = checkDecompressionRatio(pathStr, &zr.Reader)
	if err == nil {
		t.Fatal("expected ratio rejection")
	}
	if !strings.Contains(err.Error(), "ratio") {
		t.Errorf("expected ratio error; got %v", err)
	}
}

// TestCheckDecompressionRatio_NormalArchivePasses confirms a
// legitimate, modestly-compressed archive is NOT rejected.
func TestCheckDecompressionRatio_NormalArchivePasses(t *testing.T) {
	path := buildFixtureEPUB(t, defaultFixture())
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = zr.Close() }()
	if err := checkDecompressionRatio(path, &zr.Reader); err != nil {
		t.Errorf("normal EPUB rejected by ratio guard: %v", err)
	}
}

func TestExtractor_Identifies(t *testing.T) {
	e := New()
	if e.Name() != Name {
		t.Errorf("Name = %q; want %q", e.Name(), Name)
	}
	if e.Version() != Version {
		t.Errorf("Version = %q; want %q", e.Version(), Version)
	}
}

func TestSafeSectionID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"chapter-1", "001-chapter-1"},
		{"Chapter 1", "001-chapter-1"},
		{"", "section-001"},
		{"???", "section-001"},
		{"x/../escape", "001-x----escape"},
	}
	for i, c := range cases {
		got := safeSectionID(c.in, 0)
		if got != c.want {
			t.Errorf("case %d safeSectionID(%q) = %q; want %q", i, c.in, got, c.want)
		}
	}
}
