// Package epub implements the EPUB extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 1) — this is the MVP extractor, deliberately
// dependency-light (stdlib + golang.org/x/net/html).
//
// EPUB structure (per IDPF EPUB 3 spec):
//   - The .epub file is a ZIP archive.
//   - META-INF/container.xml points at the OPF (Open Packaging Format)
//     manifest, typically OEBPS/content.opf.
//   - The OPF declares the spine (reading order, sequence of XHTML
//     file references) and metadata (title, author, language, etc.).
//   - Each spine entry is an XHTML file; chapters typically map 1:1
//     to spine entries.
//
// Reading order = spine order. Section IDs are derived from the
// spine item's idref so they're stable across re-extraction.
package epub

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"vornik.io/vornik/internal/extractor"
)

const (
	// Name is the canonical identifier persisted on every
	// extracted_documents.extractor_name row this extractor produces.
	Name = "vornik-extract-epub"

	// Version follows semver-ish convention. Bump when extraction
	// logic changes in a way that justifies re-extracting historical
	// EPUBs. The DB's UNIQUE constraint on
	// (source_artifact_id, extractor_name, extractor_version) means
	// a bump produces a new row alongside the old; the old stays
	// queryable for diff comparison.
	Version = "0.1.0"

	// maxSpineItems guards against pathological EPUBs (compression
	// bombs, malicious manifests with millions of entries). 5000 is
	// well above any real book's chapter count.
	maxSpineItems = 5000

	// maxFileBytes caps per-XHTML file size to defend against ZIP-
	// bomb expansion. 10 MiB per chapter is generous (most novels
	// run 30-80 KiB per chapter); textbooks with embedded base64
	// images can run larger and we'd prefer they fail loudly here
	// than blow memory.
	maxFileBytes = 10 << 20 // 10 MiB

	// maxTotalUncompressedBytes caps the CUMULATIVE uncompressed size
	// read across every entry of a single EPUB. The per-file cap
	// alone (maxFileBytes) lets a bomb slip through as N files each
	// just under the cap; this aggregate budget is what bounds the
	// daemon's heap. batch-3 ingress/untrusted-input: document-
	// extraction hardening (a). 256 MiB covers any real book (even
	// image-heavy textbooks) while keeping the worst case well inside
	// the agent container's memory envelope.
	maxTotalUncompressedBytes = 256 << 20 // 256 MiB

	// maxDecompressionRatio is the ceiling on (declared uncompressed
	// total / on-disk archive size). A classic decompression bomb is
	// a kilobyte-scale archive that declares gigabytes uncompressed;
	// this trips up front, before any entry is streamed into memory.
	// 200:1 is far above legitimate EPUB compression (text + a few
	// images compresses ~3-10:1; even all-text runs under ~50:1).
	// batch-3 ingress/untrusted-input: document-extraction hardening (a).
	maxDecompressionRatio = 200
)

// New returns a freshly-constructed EPUB extractor.
func New() *Extractor { return &Extractor{} }

// Extractor implements extractor.Extractor for EPUB files.
// Stateless — safe to share across goroutines.
type Extractor struct{}

// Name returns the canonical extractor name.
func (*Extractor) Name() string { return Name }

// Version returns the extractor version string.
func (*Extractor) Version() string { return Version }

// Extract reads the EPUB at src.FilePath and returns its structured
// content. Errors propagate from the underlying zip / OPF parsing;
// the caller is responsible for storing whatever Metadata or
// Sections did parse before the error for diagnostic purposes.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("epub: source file path is empty")
	}
	zr, err := zip.OpenReader(src.FilePath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("epub: open zip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	// Decompression-ratio pre-check (zip-bomb guard, hardening (a)).
	// Uses the central-directory's declared uncompressed sizes vs the
	// archive's on-disk size — cheap, runs before any entry is read.
	if err := checkDecompressionRatio(src.FilePath, &zr.Reader); err != nil {
		return extractor.Result{}, fmt.Errorf("epub: %w", err)
	}

	files := indexZipFiles(&zr.Reader)

	// budget bounds the CUMULATIVE uncompressed bytes read across all
	// entries — the per-file cap alone can't stop an aggregate bomb.
	budget := newReadBudget(maxTotalUncompressedBytes)

	opfPath, err := findOPFPath(files, budget)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("epub: locate OPF: %w", err)
	}

	opf, err := readOPF(files, opfPath, budget)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("epub: read OPF: %w", err)
	}

	metadata := buildMetadata(opf, src.OriginalName)

	sections, outline, err := readSpine(ctx, files, opf, opfPath, budget)
	if err != nil {
		// Return whatever parsed so callers can still record a
		// PARTIAL row for diagnostics.
		return extractor.Result{Metadata: metadata}, fmt.Errorf("epub: read spine: %w", err)
	}

	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// zipFileMap is a lookup keyed on the in-zip path (forward slashes,
// no leading slash) so the rest of the parser doesn't have to
// re-iterate the zip's File slice for each cross-reference.
type zipFileMap map[string]*zip.File

func indexZipFiles(zr *zip.Reader) zipFileMap {
	m := make(zipFileMap, len(zr.File))
	for _, f := range zr.File {
		m[f.Name] = f
	}
	return m
}

// findOPFPath reads META-INF/container.xml to locate the OPF.
// EPUB 3 spec requires container.xml to point at "the" rootfile;
// when multiple rootfiles exist we take the first
// application/oebps-package+xml one.
func findOPFPath(files zipFileMap, budget *readBudget) (string, error) {
	containerFile, ok := files["META-INF/container.xml"]
	if !ok {
		return "", fmt.Errorf("META-INF/container.xml missing — not a valid EPUB")
	}
	data, err := readZipFile(containerFile, budget)
	if err != nil {
		return "", err
	}
	var container struct {
		Rootfiles struct {
			Rootfile []struct {
				FullPath  string `xml:"full-path,attr"`
				MediaType string `xml:"media-type,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := xml.Unmarshal(data, &container); err != nil {
		return "", fmt.Errorf("parse container.xml: %w", err)
	}
	for _, rf := range container.Rootfiles.Rootfile {
		if rf.FullPath == "" {
			continue
		}
		if rf.MediaType == "" || rf.MediaType == "application/oebps-package+xml" {
			return rf.FullPath, nil
		}
	}
	return "", fmt.Errorf("container.xml declares no rootfile")
}

// opfDocument is the subset of the OPF schema we read. Fields we
// don't need are ignored by xml.Unmarshal.
type opfDocument struct {
	Metadata struct {
		Titles      []string  `xml:"title"`
		Creators    []creator `xml:"creator"`
		Publisher   string    `xml:"publisher"`
		Date        string    `xml:"date"`
		Language    string    `xml:"language"`
		Identifiers []ident   `xml:"identifier"`
	} `xml:"metadata"`
	Manifest struct {
		Items []manifestItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		ItemRefs []spineItem `xml:"itemref"`
	} `xml:"spine"`
}

type creator struct {
	Name string `xml:",chardata"`
	Role string `xml:"role,attr"`
}

type ident struct {
	Value  string `xml:",chardata"`
	Scheme string `xml:"scheme,attr"`
}

type manifestItem struct {
	ID        string `xml:"id,attr"`
	Href      string `xml:"href,attr"`
	MediaType string `xml:"media-type,attr"`
}

type spineItem struct {
	IDRef  string `xml:"idref,attr"`
	Linear string `xml:"linear,attr"`
}

func readOPF(files zipFileMap, opfPath string, budget *readBudget) (*opfDocument, error) {
	f, ok := files[opfPath]
	if !ok {
		return nil, fmt.Errorf("OPF file %q not in archive", opfPath)
	}
	data, err := readZipFile(f, budget)
	if err != nil {
		return nil, err
	}
	var opf opfDocument
	if err := xml.Unmarshal(data, &opf); err != nil {
		return nil, fmt.Errorf("parse OPF: %w", err)
	}
	return &opf, nil
}

// buildMetadata folds the OPF metadata block into the structured
// extractor.Metadata shape. originalName is the operator-visible
// filename, used as the title fallback when the EPUB has no
// <dc:title> (rare but happens on hand-rolled ebook conversions).
func buildMetadata(opf *opfDocument, originalName string) extractor.Metadata {
	m := extractor.Metadata{}
	if len(opf.Metadata.Titles) > 0 {
		m.Title = strings.TrimSpace(opf.Metadata.Titles[0])
	}
	if m.Title == "" && originalName != "" {
		// Strip extension; keep operator-visible spelling.
		m.Title = strings.TrimSuffix(originalName, path.Ext(originalName))
	}

	// Prefer creators with role=aut (author). Fall back to the first
	// creator if no role hints are present.
	for _, c := range opf.Metadata.Creators {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		if c.Role == "aut" || c.Role == "" {
			if m.Author != "" {
				m.Author += "; "
			}
			m.Author += name
		}
	}
	m.Publisher = strings.TrimSpace(opf.Metadata.Publisher)
	m.PublicationDate = strings.TrimSpace(opf.Metadata.Date)
	m.Language = strings.TrimSpace(opf.Metadata.Language)
	for _, id := range opf.Metadata.Identifiers {
		value := strings.TrimSpace(id.Value)
		if value == "" {
			continue
		}
		if isISBN(value) || strings.EqualFold(id.Scheme, "isbn") {
			m.ISBN = value
			break
		}
	}
	return m
}

// readSpine walks the spine in reading order, resolves each idref
// to a manifest item, reads the XHTML, and extracts text into a
// markdown-ish form. Returns sections + the matching outline so
// section_count + outline are in sync.
func readSpine(ctx context.Context, files zipFileMap, opf *opfDocument, opfPath string, budget *readBudget) ([]extractor.Section, []extractor.OutlineEntry, error) {
	if len(opf.Spine.ItemRefs) == 0 {
		return nil, nil, fmt.Errorf("spine is empty")
	}
	if len(opf.Spine.ItemRefs) > maxSpineItems {
		return nil, nil, fmt.Errorf("spine has %d items (cap %d) — refusing pathological EPUB", len(opf.Spine.ItemRefs), maxSpineItems)
	}

	manifestByID := make(map[string]manifestItem, len(opf.Manifest.Items))
	for _, item := range opf.Manifest.Items {
		manifestByID[item.ID] = item
	}

	opfDir := path.Dir(opfPath)
	sections := make([]extractor.Section, 0, len(opf.Spine.ItemRefs))
	outline := make([]extractor.OutlineEntry, 0, len(opf.Spine.ItemRefs))

	for i, ref := range opf.Spine.ItemRefs {
		if err := ctx.Err(); err != nil {
			return sections, outline, err
		}
		// linear="no" entries are auxiliary (cover, copyright page,
		// front matter not in main flow). Including them keeps the
		// extraction lossless; downstream chunkers can downweight
		// based on the section_id naming.
		item, ok := manifestByID[ref.IDRef]
		if !ok {
			continue
		}
		href := path.Join(opfDir, item.Href)
		f, ok := files[href]
		if !ok {
			continue
		}
		data, err := readZipFile(f, budget)
		if err != nil {
			return sections, outline, fmt.Errorf("read %q: %w", href, err)
		}
		text, title, err := extractXHTMLText(data)
		if err != nil {
			return sections, outline, fmt.Errorf("parse %q: %w", href, err)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		sectionID := safeSectionID(ref.IDRef, i)
		if title == "" {
			title = sectionID
		}
		sections = append(sections, extractor.Section{
			SectionID: sectionID,
			Title:     title,
			Content:   text,
		})
		outline = append(outline, extractor.OutlineEntry{
			SectionID: sectionID,
			Title:     title,
			Depth:     0, // EPUB spine is flat — depth only meaningful with nav.xhtml parsing
			TextBytes: len(text),
		})
	}
	if len(sections) == 0 {
		return nil, nil, fmt.Errorf("spine yielded no extractable text")
	}
	return sections, outline, nil
}

// readBudget tracks the cumulative uncompressed bytes consumed
// across all entries of one EPUB. It is the aggregate companion to
// the per-file maxFileBytes cap: a bomb made of many sub-cap files
// is stopped here, not by the per-file LimitReader.
//
// Not safe for concurrent use — a single Extract call owns one
// budget and reads entries sequentially.
type readBudget struct {
	remaining int64
	limit     int64
}

func newReadBudget(limit int64) *readBudget {
	return &readBudget{remaining: limit, limit: limit}
}

// charge debits n bytes from the budget, returning an error once the
// cumulative total would exceed the cap. Charging is the actual
// memory-exhaustion guard; the per-read LimitReader in readZipFile
// ensures we never buffer more than maxFileBytes+1 before charging.
func (b *readBudget) charge(n int) error {
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return fmt.Errorf("cumulative uncompressed size exceeds %d byte cap (decompression bomb?)", b.limit)
	}
	return nil
}

// readZipFile reads a zip entry into memory with both a per-file
// size cap and a cumulative-budget charge. Returning early on
// cap-violation prevents a ZIP bomb from blowing the daemon's heap.
// batch-3 ingress/untrusted-input: document-extraction hardening (a).
func readZipFile(f *zip.File, budget *readBudget) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	// LimitReader caps even when the zip metadata lies about
	// uncompressed size — we never buffer more than maxFileBytes+1
	// for a single entry regardless of the cumulative budget.
	limited := io.LimitReader(rc, maxFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFileBytes {
		return nil, fmt.Errorf("file %q exceeds %d byte cap", f.Name, maxFileBytes)
	}
	if budget != nil {
		if err := budget.charge(len(data)); err != nil {
			return nil, fmt.Errorf("file %q: %w", f.Name, err)
		}
	}
	return data, nil
}

// checkDecompressionRatio rejects archives whose declared
// uncompressed total dwarfs their on-disk size — the classic
// decompression-bomb fingerprint. It also caps the absolute declared
// total at maxTotalUncompressedBytes so a large-but-low-ratio bomb is
// still refused before any entry is streamed. Both checks run up
// front using only central-directory metadata; no entry is opened.
// batch-3 ingress/untrusted-input: document-extraction hardening (a).
func checkDecompressionRatio(filePath string, zr *zip.Reader) error {
	var declared uint64
	for _, f := range zr.File {
		declared += f.UncompressedSize64
	}
	if declared > uint64(maxTotalUncompressedBytes) {
		return fmt.Errorf("declared uncompressed size %d exceeds %d byte cap (decompression bomb?)", declared, maxTotalUncompressedBytes)
	}
	st, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("stat archive: %w", err)
	}
	onDisk := st.Size()
	if onDisk <= 0 {
		return nil // empty/odd archive — downstream parse will reject
	}
	if declared/uint64(onDisk) > uint64(maxDecompressionRatio) {
		return fmt.Errorf("decompression ratio %d:1 exceeds %d:1 cap (decompression bomb?)", declared/uint64(onDisk), maxDecompressionRatio)
	}
	return nil
}

// extractXHTMLText walks the XHTML body and emits markdown-ish
// text. The conversion is deliberately simple — h1/h2/h3 become
// `# / ## / ###` headings; p / li / blockquote stay flat; everything
// else falls through as text. The first heading found becomes the
// section title.
func extractXHTMLText(data []byte) (text string, title string, err error) {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return "", "", err
	}
	var sb strings.Builder
	walkXHTML(doc, &sb, &title)
	// Collapse runs of blank lines.
	out := collapseBlankLines(sb.String())
	return strings.TrimSpace(out), strings.TrimSpace(title), nil
}

// walkXHTML is the recursive node walker. firstTitle captures the
// text content of the first <h1>/<h2>/<h3> we encounter; we use
// pointer-to-string so the caller can read it after the walk
// returns.
func walkXHTML(n *html.Node, sb *strings.Builder, firstTitle *string) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.ElementNode:
		tag := strings.ToLower(n.Data)
		switch tag {
		case "script", "style":
			return // never include script bodies or CSS
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level, _ := strconv.Atoi(strings.TrimPrefix(tag, "h"))
			if level == 0 {
				level = 1
			}
			headingText := extractTextOnly(n)
			if *firstTitle == "" {
				*firstTitle = headingText
			}
			sb.WriteString("\n")
			sb.WriteString(strings.Repeat("#", level))
			sb.WriteString(" ")
			sb.WriteString(headingText)
			sb.WriteString("\n\n")
			return // don't descend; we already pulled text
		case "p", "blockquote", "li", "pre":
			text := extractTextOnly(n)
			if text != "" {
				switch tag {
				case "blockquote":
					for _, line := range strings.Split(text, "\n") {
						sb.WriteString("> ")
						sb.WriteString(line)
						sb.WriteString("\n")
					}
					sb.WriteString("\n")
				case "li":
					sb.WriteString("- ")
					sb.WriteString(text)
					sb.WriteString("\n")
				default:
					sb.WriteString(text)
					sb.WriteString("\n\n")
				}
			}
			return
		case "br":
			sb.WriteString("\n")
			return
		}
	case html.TextNode:
		// Inline text outside block elements — emit verbatim. The
		// block-element handlers above pull their own text via
		// extractTextOnly, so we only reach here for stray text in
		// <body> directly.
		if strings.TrimSpace(n.Data) != "" {
			sb.WriteString(n.Data)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkXHTML(c, sb, firstTitle)
	}
}

// extractTextOnly returns the concatenated text content of a node
// without further markdown formatting — used by walkXHTML to pull
// the content of headings, paragraphs, list items, etc.
func extractTextOnly(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" {
				return
			}
			if tag == "br" {
				sb.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(collapseInlineWhitespace(sb.String()))
}

// collapseInlineWhitespace turns runs of whitespace (including
// newlines from XHTML formatting) into single spaces. Preserves
// explicit \n that callers emit for <br>.
func collapseInlineWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !prevSpace {
				sb.WriteByte(' ')
				prevSpace = true
			}
		default:
			sb.WriteRune(r)
			prevSpace = false
		}
	}
	return sb.String()
}

// collapseBlankLines reduces runs of 2+ blank lines to a single
// blank line. Keeps markdown paragraph separation clean.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks <= 1 {
				out = append(out, "")
			}
		} else {
			blanks = 0
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// safeSectionID turns an OPF idref into a filesystem-safe identifier.
// Falls back to "section-NNN" when the idref is missing or has no
// alphanumeric content (some EPUBs use numeric-only or empty ids).
func safeSectionID(idref string, index int) string {
	idref = strings.TrimSpace(idref)
	if idref == "" {
		return fmt.Sprintf("section-%03d", index+1)
	}
	var sb strings.Builder
	sb.Grow(len(idref))
	for _, r := range idref {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	id := strings.ToLower(strings.Trim(sb.String(), "-"))
	if id == "" {
		return fmt.Sprintf("section-%03d", index+1)
	}
	// Prepend the index for stable ordering even when idrefs are
	// non-unique across the spine (legal EPUB but rare).
	return fmt.Sprintf("%03d-%s", index+1, id)
}

// isISBN does a coarse digit-count check (ISBN-10 / ISBN-13) on a
// candidate string — good enough to disambiguate which identifier
// to surface as Metadata.ISBN when the OPF lists several without
// a scheme attribute.
func isISBN(s string) bool {
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits == 10 || digits == 13
}
