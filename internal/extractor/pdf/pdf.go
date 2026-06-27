// Package pdf implements the PDF extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 3) — text-extractable PDFs only; OCR fallback for
// scanned PDFs lands with Phase 5.
//
// Approach: shell out to poppler's pdftotext binary (a host
// dependency, same as our reliance on jq/git inside containers).
// Page boundaries are preserved via the form-feed (\x0c) bytes
// pdftotext emits by default, which we split on to produce one
// section per page.
//
// Why not a pure-Go library: pdfcpu/ledongthuc/dslipak all fall
// short on the long tail of PDFs in the wild — embedded fonts,
// CID encodings, content streams that use Tj/TJ operators with
// custom encoding maps. poppler has 20+ years of incremental
// fixes for those edge cases. The performance + correctness
// gap is wider than the convenience win of staying in-process.
package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"vornik.io/vornik/internal/extractor"
)

const (
	// Name is the canonical extractor identifier persisted on
	// every extracted_documents.extractor_name row. The fixed
	// "vornik-extract-pdf" matches the design-doc naming.
	Name = "vornik-extract-pdf"

	// Version follows semver-ish. Bump when extraction logic
	// changes meaningfully — e.g. when we add TOC-based section
	// promotion or OCR fallback.
	Version = "0.1.0"

	// maxPDFPages caps the per-document section count. A PDF
	// claiming 50,000 pages is either a poppler bug, a malicious
	// payload, or an actual book that needs operator follow-up
	// (split before ingestion). 5000 covers any real textbook
	// or research compilation.
	maxPDFPages = 5000
)

// New returns a freshly-constructed PDF extractor.
func New() *Extractor { return &Extractor{} }

// NewWithBinary lets tests inject a different binary path / args
// without touching $PATH. Production code uses New().
func NewWithBinary(path string) *Extractor { return &Extractor{binaryPath: path} }

// Extractor implements extractor.Extractor for PDF files via the
// poppler pdftotext binary. Stateless; safe across goroutines.
type Extractor struct {
	binaryPath string // empty = look up "pdftotext" on PATH
}

// Name returns the canonical extractor name.
func (*Extractor) Name() string { return Name }

// Version returns the extractor version string.
func (*Extractor) Version() string { return Version }

// Extract runs pdftotext on the source file and splits the output
// on form-feed page boundaries to produce one section per page.
// Returns a structured Result with one Section per page; the
// outline mirrors sections 1:1 with PageStart populated for
// citation-friendly retrieval.
//
// Errors:
//   - pdftotext binary missing on PATH → fail fast so the operator
//     sees a clear "install poppler-utils" message rather than a
//     confusing exec error deep in the stack.
//   - pdftotext exit code != 0 → wrap stderr verbatim.
//   - Zero text extracted (scanned PDF) → return ErrNoTextExtracted
//     so callers can route to the OCR fallback when it lands.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("pdf: source file path is empty")
	}

	binary := e.binaryPath
	if binary == "" {
		binary = "pdftotext"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("pdf: %s not found on PATH (install poppler-utils): %w", binary, err)
	}

	// pdftotext flags:
	//   -enc UTF-8     — force UTF-8 output (default is sometimes ASCII)
	//   -nopgbrk       — REMOVED. We rely on form-feed (\x0c) chars
	//                    between pages for section splits, so we must
	//                    NOT pass this flag.
	//   -q             — suppress informational stderr noise; real
	//                    errors still surface via non-zero exit.
	//   <input> -      — read from path, write to stdout.
	cmd := exec.CommandContext(ctx, resolved, "-enc", "UTF-8", "-q", src.FilePath, "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface stderr verbatim so the daemon log shows
		// poppler's diagnostic ("Syntax Error: invalid xref", etc.)
		// rather than just "exit status 1".
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return extractor.Result{}, fmt.Errorf("pdf: pdftotext failed: %s", msg)
	}

	pages := splitPages(stdout.Bytes())
	// pdftotext often emits a trailing \x0c after the last page,
	// yielding an empty final entry from the split. Strip trailing
	// all-whitespace pages so PageCount reflects real pages, not
	// the splitter's accounting.
	for len(pages) > 0 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 0 {
		return extractor.Result{}, ErrNoTextExtracted
	}
	if len(pages) > maxPDFPages {
		return extractor.Result{}, fmt.Errorf("pdf: %d pages exceeds cap %d — split source before ingest", len(pages), maxPDFPages)
	}

	metadata := buildMetadata(src.OriginalName)
	metadata.PageCount = len(pages)

	sections := make([]extractor.Section, 0, len(pages))
	outline := make([]extractor.OutlineEntry, 0, len(pages))
	nonEmpty := 0
	for i, page := range pages {
		text := strings.TrimSpace(page)
		if text == "" {
			continue
		}
		nonEmpty++
		sectionID := fmt.Sprintf("page-%04d", i+1)
		title := summarisePage(text, i+1)
		sections = append(sections, extractor.Section{
			SectionID: sectionID,
			Title:     title,
			Content:   text,
		})
		outline = append(outline, extractor.OutlineEntry{
			SectionID: sectionID,
			Title:     title,
			Depth:     0,
			PageStart: i + 1,
			TextBytes: len(text),
		})
	}
	if nonEmpty == 0 {
		// Every page parsed but produced no text — the canonical
		// scanned-PDF signal. Return a sentinel so future Phase-5
		// callers can re-route to OCR without re-parsing.
		return extractor.Result{Metadata: metadata}, ErrNoTextExtracted
	}

	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// ErrNoTextExtracted means the PDF parsed but contained no
// extractable text. Typical cause: scanned-image PDFs where the
// pages are bitmaps with no text layer. Caller-visible so the
// future OCR fallback can route to a different extractor.
var ErrNoTextExtracted = errors.New("pdf: no text content extracted (likely scanned/image PDF — OCR fallback not yet available)")

// splitPages divides pdftotext output on form-feed bytes (\x0c).
// pdftotext emits \x0c between pages by default; the last page
// has no trailing form-feed so we don't drop trailing empties.
//
// We return [][]byte so callers can inspect raw bytes; the section
// builder above converts to string after TrimSpace.
func splitPages(data []byte) []string {
	parts := bytes.Split(data, []byte{0x0c})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out
}

// summarisePage builds a short, deterministic section title from
// the page's leading text. PDFs don't carry per-page titles, so
// we take the first non-empty line, truncated to 80 chars. Falls
// back to "Page N" when the page starts with only whitespace.
//
// Quality matters: this title shows up in memory_search results
// + document_get_outline responses. A clean "Chapter 4 — Schema
// Mode Mapping" beats "[binary garbage]" or "Page 47".
func summarisePage(text string, pageNumber int) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if len(trimmed) > 80 {
			trimmed = trimmed[:80] + "…"
		}
		return trimmed
	}
	return fmt.Sprintf("Page %d", pageNumber)
}

// buildMetadata populates the fields PDF can offer up-front. Title
// falls back to the operator-visible filename (extension stripped)
// when the PDF doesn't carry document properties. Reading the
// embedded XMP / Info dictionary for the real title is a Phase-3b
// nice-to-have; for now the filename heuristic matches user
// expectation when forwarding "<paper-name>.pdf".
func buildMetadata(originalName string) extractor.Metadata {
	m := extractor.Metadata{}
	if originalName != "" {
		title := originalName
		if i := strings.LastIndex(title, "."); i > 0 {
			title = title[:i]
		}
		m.Title = title
	}
	return m
}
