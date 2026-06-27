// Tests for the plain-text + markdown extractor.
package textfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
)

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtract_PlainText_SingleSection(t *testing.T) {
	body := "Line one.\nLine two.\nLine three."
	path := writeFile(t, "notes.txt", body)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "text/plain",
		OriginalName: "notes.txt",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1 for plain text", len(res.Sections))
	}
	if !strings.Contains(res.Sections[0].Content, "Line one.") {
		t.Errorf("content lost original lines: %q", res.Sections[0].Content)
	}
	if res.Metadata.Title != "notes" {
		t.Errorf("Title = %q; want \"notes\" (filename minus ext)", res.Metadata.Title)
	}
	if res.Sections[0].SectionID != "001-body" {
		t.Errorf("section ID = %q; want 001-body", res.Sections[0].SectionID)
	}
}

func TestExtract_Markdown_SplitsByHeadings(t *testing.T) {
	body := `# Introduction

Opening paragraph.

## Background

Background text.

## Methods

Methods text.
` + "```" + `
code block stays here
` + "```" + `

### Sub-heading (H3 stays inline)

Inline detail.
`
	path := writeFile(t, "doc.md", body)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "text/markdown",
		OriginalName: "doc.md",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	wantTitles := []string{"Introduction", "Background", "Methods"}
	if len(res.Sections) != len(wantTitles) {
		t.Fatalf("sections = %d; want %d", len(res.Sections), len(wantTitles))
	}
	for i, want := range wantTitles {
		if res.Sections[i].Title != want {
			t.Errorf("sections[%d].Title = %q; want %q", i, res.Sections[i].Title, want)
		}
	}
	// H3 stayed inline in the "Methods" section.
	if !strings.Contains(res.Sections[2].Content, "### Sub-heading") {
		t.Errorf("Methods section dropped H3 child; got %q", res.Sections[2].Content)
	}
	if !strings.Contains(res.Sections[2].Content, "code block stays here") {
		t.Errorf("Methods section lost code block: %q", res.Sections[2].Content)
	}
}

func TestExtract_Markdown_NoHeadings_FallsBackToSingleSection(t *testing.T) {
	body := "Just some\nplain markdown\nwithout any headings."
	path := writeFile(t, "notes.md", body)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath: path, MimeType: "text/markdown", OriginalName: "notes.md",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1 for no-headings markdown", len(res.Sections))
	}
	if res.Sections[0].Title != "notes" {
		t.Errorf("Title fallback = %q; want \"notes\"", res.Sections[0].Title)
	}
}

func TestExtract_DetectsMarkdownByFilename(t *testing.T) {
	// MIME type missing — extension carries the signal.
	body := "# Heading\n\nbody"
	path := writeFile(t, "README.md", body)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath: path, OriginalName: "README.md",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Sections[0].Title != "Heading" {
		t.Errorf("markdown-by-extension lost heading title; got %q", res.Sections[0].Title)
	}
}

func TestExtract_EmptyFile_Errors(t *testing.T) {
	path := writeFile(t, "empty.txt", "")
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-file error; got %v", err)
	}
}

func TestExtract_WhitespaceOnly_Errors(t *testing.T) {
	path := writeFile(t, "blank.md", "\n\n   \n\n")
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil {
		t.Error("whitespace-only file must error")
	}
}

func TestExtract_OversizeRejected(t *testing.T) {
	// Synthesise a file just above the cap — read should reject
	// before the section builder runs.
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(path, make([]byte, maxTextBytes+1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected cap-exceeded error; got %v", err)
	}
}

func TestIsMarkdown(t *testing.T) {
	cases := []struct {
		src  extractor.Source
		want bool
	}{
		{extractor.Source{MimeType: "text/markdown"}, true},
		{extractor.Source{MimeType: "text/x-markdown"}, true},
		{extractor.Source{OriginalName: "x.md"}, true},
		{extractor.Source{OriginalName: "x.markdown"}, true},
		{extractor.Source{OriginalName: "x.txt"}, false},
		{extractor.Source{MimeType: "text/plain"}, false},
		{extractor.Source{}, false},
	}
	for i, c := range cases {
		if got := isMarkdown(c.src); got != c.want {
			t.Errorf("case %d: isMarkdown(%+v) = %v; want %v", i, c.src, got, c.want)
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
