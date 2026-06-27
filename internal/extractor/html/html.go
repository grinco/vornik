// Package html implements the HTML extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 3).
//
// Approach: pure Go using golang.org/x/net/html (already a dep via
// the EPUB extractor). HTML files are smaller and simpler than
// EPUBs — we just walk the body, strip script/style/nav, and emit
// a single section per heading boundary so the LLM can navigate
// long pages (Wikipedia articles, research write-ups) the same
// way it navigates an EPUB.
//
// Section boundaries: each top-level <h1> through <h6> starts a
// new section. Content between headings is folded into the
// preceding section's body. When the document has no headings,
// the whole body becomes one section named after <title>.
package html

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"vornik.io/vornik/internal/extractor"
)

const (
	Name    = "vornik-extract-html"
	Version = "0.1.0"

	// maxHTMLBytes caps the in-memory parse size. 8 MiB covers any
	// reasonable single-page document; pathological pages with
	// minified inline data URLs (base64 images) blow past this.
	// Caller can split or strip before re-ingesting.
	maxHTMLBytes = 8 << 20
)

func New() *Extractor { return &Extractor{} }

type Extractor struct{}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract reads the HTML at src.FilePath, parses it, and emits
// one Section per heading boundary. Returns one section named
// "Body" with the whole content when no headings are present —
// the LLM still benefits from the markdown-ish formatting + the
// chunker still splits long sections downstream.
func (e *Extractor) Extract(_ context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("html: source file path is empty")
	}
	f, err := os.Open(src.FilePath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("html: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// LimitReader so a 500-MiB malicious page can't blow daemon
	// memory. +1 lets us detect the cap-bust.
	limited := io.LimitReader(f, maxHTMLBytes+1)
	doc, err := html.Parse(limited)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("html: parse: %w", err)
	}

	title, body := findTitleAndBody(doc)
	if body == nil {
		return extractor.Result{}, fmt.Errorf("html: no <body> element found")
	}

	metadata := extractor.Metadata{}
	switch {
	case strings.TrimSpace(title) != "":
		metadata.Title = strings.TrimSpace(title)
	case src.OriginalName != "":
		// Strip extension as the title fallback. Common for
		// emailed HTML attachments whose <title> is generic
		// ("Document", "Untitled").
		fallback := src.OriginalName
		if i := strings.LastIndex(fallback, "."); i > 0 {
			fallback = fallback[:i]
		}
		metadata.Title = fallback
	}
	if lang := documentLanguage(doc); lang != "" {
		metadata.Language = lang
	}

	sections := buildSections(body, metadata.Title)
	if len(sections) == 0 {
		return extractor.Result{Metadata: metadata}, fmt.Errorf("html: body has no extractable text")
	}

	outline := make([]extractor.OutlineEntry, 0, len(sections))
	for _, s := range sections {
		outline = append(outline, extractor.OutlineEntry{
			SectionID: s.SectionID,
			Title:     s.Title,
			Depth:     0, // depth is best-effort from heading level; flat for v1
			TextBytes: len(s.Content),
		})
	}
	return extractor.Result{Metadata: metadata, Outline: outline, Sections: sections}, nil
}

// findTitleAndBody walks the parse tree looking for the <title>
// element (under <head>) and the <body>. We avoid the assumption
// that there's exactly one — html.Parse always wraps the input
// in implicit <html>/<head>/<body> nodes, so the lookup is just
// a tree walk.
func findTitleAndBody(n *html.Node) (title string, body *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "title":
				title = nodeText(n)
			case "body":
				body = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return
}

// documentLanguage reads the <html lang="..."> attribute.
// Returns "" when absent or empty.
func documentLanguage(n *html.Node) string {
	if n == nil {
		return ""
	}
	if n.Type == html.ElementNode && strings.ToLower(n.Data) == "html" {
		for _, a := range n.Attr {
			if strings.ToLower(a.Key) == "lang" {
				return strings.TrimSpace(a.Val)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if v := documentLanguage(c); v != "" {
			return v
		}
	}
	return ""
}

// nodeText returns the concatenated text content of all text-node
// descendants, collapsed to single-space whitespace.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return collapseWhitespace(sb.String())
}

// buildSections walks the body, splitting on heading tags. Each
// heading starts a new section; the section ID is derived from
// the heading text (lowercase + hyphen-sanitised) with a 3-digit
// reading-order prefix for stability.
//
// fallbackTitle is used when the body has no headings at all —
// we emit a single section named after the document title.
func buildSections(body *html.Node, fallbackTitle string) []extractor.Section {
	var (
		out        []extractor.Section
		currentTxt strings.Builder
		curID      string
		curTitle   string
		idx        = 0
	)
	flush := func() {
		text := strings.TrimSpace(currentTxt.String())
		if text == "" {
			return
		}
		if curID == "" {
			idx++
			curID = sectionIDFromTitle(fallbackTitle, idx)
			curTitle = fallbackTitle
			if curTitle == "" {
				curTitle = "Body"
			}
		}
		out = append(out, extractor.Section{
			SectionID: curID,
			Title:     curTitle,
			Content:   text,
		})
		currentTxt.Reset()
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			switch tag {
			case "script", "style", "nav", "header", "footer", "aside":
				// Boilerplate / navigation — skip wholesale.
				return
			case "h1", "h2", "h3", "h4", "h5", "h6":
				// Heading flushes the current section and opens a new one.
				flush()
				level, _ := strconv.Atoi(strings.TrimPrefix(tag, "h"))
				if level == 0 {
					level = 1
				}
				headingText := strings.TrimSpace(nodeText(n))
				idx++
				curID = sectionIDFromTitle(headingText, idx)
				curTitle = headingText
				currentTxt.WriteString(strings.Repeat("#", level))
				currentTxt.WriteString(" ")
				currentTxt.WriteString(headingText)
				currentTxt.WriteString("\n\n")
				return
			case "p", "blockquote", "li":
				txt := strings.TrimSpace(nodeText(n))
				if txt != "" {
					prefix := ""
					switch tag {
					case "blockquote":
						prefix = "> "
					case "li":
						prefix = "- "
					}
					for _, line := range strings.Split(txt, "\n") {
						if strings.TrimSpace(line) == "" {
							continue
						}
						currentTxt.WriteString(prefix)
						currentTxt.WriteString(line)
						currentTxt.WriteString("\n")
					}
					currentTxt.WriteString("\n")
				}
				return
			case "br":
				currentTxt.WriteString("\n")
				return
			}
		}
		if n.Type == html.TextNode {
			if strings.TrimSpace(n.Data) != "" {
				currentTxt.WriteString(n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	flush()
	return out
}

// sectionIDFromTitle produces a filesystem-safe section id derived
// from the heading text. Prefixed with a 3-digit index to keep
// section order stable when sorted lexically (matches the EPUB
// extractor's convention).
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

// collapseWhitespace replaces runs of whitespace with a single
// space. Same shape the EPUB extractor uses.
func collapseWhitespace(s string) string {
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
	return strings.TrimSpace(sb.String())
}
