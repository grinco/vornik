// Tests for the PDF extractor. We can't synthesize a real PDF
// fixture inline (poppler+the file format are too complex), so
// the strategy is two-pronged:
//
//   - Unit-test the parts that don't need pdftotext: page-split,
//     title summarisation, metadata fallback.
//   - Skip the binary-driven happy-path test when pdftotext is
//     missing; on hosts that DO have it (the daemon host, our CI
//     image), exercise the full Extract path with a tiny known
//     PDF written from ghostscript's "hello world" template.
package pdf

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
)

// helloPDF is a minimal 1-page PDF with the literal text "Hello,
// PDF world." written using PDF's native Tj operator. Generated
// once by hand following the PDF 1.4 spec; produces ~600 bytes
// on disk. Lets the happy-path test run against real pdftotext
// without a runtime fixture-generator.
//
// Structure:
//   - Catalog → Pages tree (1 page)
//   - Helvetica font dictionary
//   - Single content stream with BT/Tj/ET
//   - Cross-reference table + trailer
//
// The whitespace inside the content stream is significant — PDF
// is whitespace-sensitive between operators.
const helloPDF = "%PDF-1.4\n" +
	"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
	"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
	"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>\nendobj\n" +
	"4 0 obj\n<< /Length 56 >>\nstream\nBT /F1 24 Tf 100 700 Td (Hello, PDF world.) Tj ET\nendstream\nendobj\n" +
	"5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n" +
	"xref\n0 6\n0000000000 65535 f \n0000000010 00000 n \n0000000054 00000 n \n0000000100 00000 n \n0000000208 00000 n \n0000000310 00000 n \n" +
	"trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n378\n%%EOF\n"

// requirePDFTotext skips the test when pdftotext is missing on
// PATH. CI runs with poppler-utils installed; developer laptops
// might not.
func requirePDFTotext(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not on PATH; skipping (install poppler-utils to enable)")
	}
}

func writeHelloPDF(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(helloPDF), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func TestExtract_HappyPath(t *testing.T) {
	requirePDFTotext(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.pdf")
	writeHelloPDF(t, path)

	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "application/pdf",
		OriginalName: "hello.pdf",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1 (single-page fixture)", len(res.Sections))
	}
	if !strings.Contains(res.Sections[0].Content, "Hello") {
		t.Errorf("content missing fixture text; got %q", res.Sections[0].Content)
	}
	if res.Sections[0].SectionID != "page-0001" {
		t.Errorf("section_id = %q; want page-0001", res.Sections[0].SectionID)
	}
	if res.Outline[0].PageStart != 1 {
		t.Errorf("outline PageStart = %d; want 1", res.Outline[0].PageStart)
	}
	if res.Metadata.PageCount != 1 {
		t.Errorf("Metadata.PageCount = %d; want 1", res.Metadata.PageCount)
	}
	// Title falls back to filename minus extension when the PDF
	// carries no document properties.
	if res.Metadata.Title != "hello" {
		t.Errorf("Metadata.Title = %q; want \"hello\"", res.Metadata.Title)
	}
}

func TestExtract_MissingBinary_FailsFastWithGuidance(t *testing.T) {
	// Inject a binary name that won't exist on any host.
	ext := NewWithBinary("pdftotext-deliberately-missing-xyz")
	_, err := ext.Extract(context.Background(), extractor.Source{
		FilePath: "/tmp/anything.pdf",
		MimeType: "application/pdf",
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "install poppler-utils") {
		t.Errorf("error must mention the install hint; got %v", err)
	}
}

func TestExtract_EmptyFilePath(t *testing.T) {
	_, err := New().Extract(context.Background(), extractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

func TestExtract_PdftotextFailure_SurfacesStderr(t *testing.T) {
	requirePDFTotext(t)
	// Garbage input — pdftotext will exit non-zero with a clear
	// "Syntax Error" stderr line. The error must propagate so the
	// daemon log shows the diagnostic.
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.pdf")
	if err := os.WriteFile(path, []byte("this is not a PDF at all"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil {
		t.Fatal("expected error from pdftotext on garbage input")
	}
	if !strings.Contains(err.Error(), "pdftotext") {
		t.Errorf("error should mention pdftotext; got %v", err)
	}
}

func TestSplitPages(t *testing.T) {
	// pdftotext emits \x0c between pages. Verify our splitter
	// produces N pages for N-1 form-feed separators.
	cases := []struct {
		in       string
		wantLen  int
		wantP0   string
		wantP1   string
		wantLast string
	}{
		{"page1", 1, "page1", "", "page1"},
		{"page1\x0cpage2", 2, "page1", "page2", "page2"},
		{"a\x0cb\x0cc", 3, "a", "b", "c"},
		{"\x0c", 2, "", "", ""}, // empty pages on either side
	}
	for i, c := range cases {
		pages := splitPages([]byte(c.in))
		if len(pages) != c.wantLen {
			t.Errorf("case %d: got %d pages, want %d", i, len(pages), c.wantLen)
			continue
		}
		if pages[0] != c.wantP0 {
			t.Errorf("case %d: pages[0] = %q, want %q", i, pages[0], c.wantP0)
		}
		if c.wantLen > 1 && pages[1] != c.wantP1 {
			t.Errorf("case %d: pages[1] = %q, want %q", i, pages[1], c.wantP1)
		}
		if pages[len(pages)-1] != c.wantLast {
			t.Errorf("case %d: pages[last] = %q, want %q", i, pages[len(pages)-1], c.wantLast)
		}
	}
}

func TestSummarisePage(t *testing.T) {
	cases := []struct {
		page, want string
		pageNum    int
	}{
		{"Chapter 4 — Schema Mode Mapping\n\nbody body body", "Chapter 4 — Schema Mode Mapping", 4},
		{"   \n   \n\nHello world\nmore text", "Hello world", 1},
		// Truncates a long heading + adds ellipsis.
		{strings.Repeat("x", 200) + "\nrest", strings.Repeat("x", 80) + "…", 7},
		// All whitespace falls back to "Page N".
		{"   \n  \n\n", "Page 12", 12},
	}
	for i, c := range cases {
		got := summarisePage(c.page, c.pageNum)
		if got != c.want {
			t.Errorf("case %d: summarisePage = %q, want %q", i, got, c.want)
		}
	}
}

func TestBuildMetadata_TitleFromFilename(t *testing.T) {
	cases := map[string]string{
		"paper.pdf":                "paper",
		"long.title.with.dots.pdf": "long.title.with.dots",
		"no_extension":             "no_extension",
		"":                         "",
	}
	for in, want := range cases {
		got := buildMetadata(in)
		if got.Title != want {
			t.Errorf("buildMetadata(%q).Title = %q; want %q", in, got.Title, want)
		}
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
