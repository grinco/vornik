// Package textfile implements the extractor for plain text and
// markdown files. See document-extraction-design.md §11 (Phase 3).
//
// Approach: pure Go. For text/plain we emit a single section
// containing the whole document. For text/markdown we split on
// H1/H2 boundaries so memory_search can cite a specific section
// later (e.g. "from <doc> · Authoring guidelines"), matching the
// pattern the HTML extractor uses.
package textfile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
)

const (
	Name    = "vornik-extract-text"
	Version = "0.1.0"

	// maxTextBytes caps the in-memory read. 16 MiB covers any
	// reasonable plain-text upload; book-length text (multi-MB
	// novels) sits comfortably under.
	maxTextBytes = 16 << 20
)

func New() *Extractor { return &Extractor{} }

type Extractor struct{}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract reads the file and decides on a section model from the
// MIME type:
//
//   - text/markdown → one section per H1/H2 boundary
//   - text/plain (and everything else routed here) → one section
//
// The body is preserved verbatim; the file is already text so
// there's no conversion step. Validation is content-shape only
// (non-empty, under the size cap).
func (e *Extractor) Extract(_ context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("textfile: source file path is empty")
	}
	f, err := os.Open(src.FilePath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("textfile: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxTextBytes+1))
	if err != nil {
		return extractor.Result{}, fmt.Errorf("textfile: read: %w", err)
	}
	if int64(len(data)) > maxTextBytes {
		return extractor.Result{}, fmt.Errorf("textfile: file exceeds %d-byte cap", maxTextBytes)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return extractor.Result{}, fmt.Errorf("textfile: file is empty")
	}

	metadata := extractor.Metadata{}
	if title := titleFromSource(src); title != "" {
		metadata.Title = title
	}

	var sections []extractor.Section
	if isMarkdown(src) {
		sections = splitMarkdownByHeadings(content, metadata.Title)
	} else {
		sections = []extractor.Section{{
			SectionID: "001-body",
			Title:     fallback(metadata.Title, "Body"),
			Content:   content,
		}}
	}

	outline := make([]extractor.OutlineEntry, 0, len(sections))
	for _, s := range sections {
		outline = append(outline, extractor.OutlineEntry{
			SectionID: s.SectionID,
			Title:     s.Title,
			TextBytes: len(s.Content),
		})
	}
	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// isMarkdown checks both the MIME type (canonical) and the file
// extension (fallback). text/x-markdown variants exist in the
// wild — any path containing "markdown" qualifies.
func isMarkdown(src extractor.Source) bool {
	if mime := strings.ToLower(src.MimeType); strings.Contains(mime, "markdown") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(src.OriginalName))
	return ext == ".md" || ext == ".markdown"
}

// titleFromSource pulls a clean title from the operator-visible
// filename. Extension stripped; underscores/hyphens left alone
// because they often carry meaning ("2024-q3-recap" reads
// naturally as a title).
func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// fallback returns primary when non-empty, otherwise alt.
func fallback(primary, alt string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return alt
}

// splitMarkdownByHeadings divides the markdown content into
// sections at every H1 or H2 boundary. Subsequent H3-H6 stay
// inline within their parent section. Why only H1/H2: H3+
// produces too many sections on technical docs (every code
// example might have its own H3); H1/H2 mirrors the natural
// "chapter / section" granularity readers expect.
//
// When the document has no H1/H2 at all, we emit a single
// section.
func splitMarkdownByHeadings(content, fallbackTitle string) []extractor.Section {
	lines := strings.Split(content, "\n")
	var (
		out      []extractor.Section
		curBody  strings.Builder
		curTitle string
		idx      int
	)
	flush := func() {
		body := strings.TrimSpace(curBody.String())
		if body == "" {
			return
		}
		idx++
		title := curTitle
		if title == "" {
			title = fallback(fallbackTitle, "Body")
		}
		out = append(out, extractor.Section{
			SectionID: sectionIDFromTitle(title, idx),
			Title:     title,
			Content:   body,
		})
		curBody.Reset()
	}

	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		// Recognise ATX-style headings only (#, ##). Setext
		// (===, ---) is rare in modern markdown; skipping
		// keeps the splitter simple.
		switch {
		case strings.HasPrefix(stripped, "# ") || stripped == "#":
			flush()
			curTitle = strings.TrimSpace(strings.TrimPrefix(stripped, "#"))
			curBody.WriteString(line)
			curBody.WriteString("\n")
		case strings.HasPrefix(stripped, "## ") || stripped == "##":
			flush()
			curTitle = strings.TrimSpace(strings.TrimPrefix(stripped, "##"))
			curBody.WriteString(line)
			curBody.WriteString("\n")
		default:
			curBody.WriteString(line)
			curBody.WriteString("\n")
		}
	}
	flush()
	if len(out) == 0 {
		// File had no recognisable headings AND no body — should
		// not happen because Extract already rejected empty input,
		// but stay defensive.
		out = []extractor.Section{{
			SectionID: "001-body",
			Title:     fallback(fallbackTitle, "Body"),
			Content:   strings.TrimSpace(content),
		}}
	}
	return out
}

// sectionIDFromTitle mirrors the HTML/EPUB extractor's
// sanitiser — lowercase alnum + hyphens, capped at 40 chars,
// prefixed with a 3-digit reading-order index.
func sectionIDFromTitle(title string, index int) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Sprintf("section-%03d", index)
	}
	var sb strings.Builder
	sb.Grow(len(title))
	prevDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				sb.WriteByte('-')
				prevDash = true
			}
		}
	}
	id := strings.Trim(sb.String(), "-")
	if id == "" {
		return fmt.Sprintf("section-%03d", index)
	}
	if len(id) > 40 {
		id = id[:40]
	}
	return fmt.Sprintf("%03d-%s", index, id)
}
