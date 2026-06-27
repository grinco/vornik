// Tests for the HTML extractor. Real fixture content: small
// inline HTML strings exercising the four invariants the Phase-3
// design pins —
//   - title extraction
//   - script/style/nav stripping
//   - heading-driven section splits
//   - no-heading fallback to a single "Body" section
package html

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
)

func writeHTML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.html")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtract_HappyPath_HeadingsDrivenSections(t *testing.T) {
	path := writeHTML(t, `<!doctype html>
<html lang="en-US">
<head><title>Test Article</title></head>
<body>
  <nav>skip me — site nav</nav>
  <h1>Introduction</h1>
  <p>Opening paragraph with <em>emphasis</em>.</p>
  <h2>Background</h2>
  <p>Background paragraph one.</p>
  <p>Background paragraph two.</p>
  <h2>Methods</h2>
  <blockquote>A quoted insight.</blockquote>
  <ul><li>Point A</li><li>Point B</li></ul>
  <script>alert('xss');</script>
  <footer>also skip</footer>
</body>
</html>`)

	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		MimeType:     "text/html",
		OriginalName: "fixture.html",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Title != "Test Article" {
		t.Errorf("Title = %q", res.Metadata.Title)
	}
	if res.Metadata.Language != "en-US" {
		t.Errorf("Language = %q", res.Metadata.Language)
	}
	if len(res.Sections) != 3 {
		t.Fatalf("sections = %d; want 3 (one per heading)", len(res.Sections))
	}
	// Reading order preserved.
	wantTitles := []string{"Introduction", "Background", "Methods"}
	for i, want := range wantTitles {
		if res.Sections[i].Title != want {
			t.Errorf("sections[%d].Title = %q; want %q", i, res.Sections[i].Title, want)
		}
	}
	// Section IDs ordered + sanitised.
	if !strings.HasPrefix(res.Sections[0].SectionID, "001-") {
		t.Errorf("section[0].ID = %q; want 001- prefix", res.Sections[0].SectionID)
	}
	// Boilerplate stripped.
	for _, s := range res.Sections {
		if strings.Contains(s.Content, "skip me") || strings.Contains(s.Content, "also skip") {
			t.Errorf("section %q leaked nav/footer: %q", s.SectionID, s.Content)
		}
		if strings.Contains(s.Content, "alert(") {
			t.Errorf("section %q leaked <script>: %q", s.SectionID, s.Content)
		}
	}
	// Block elements rendered markdown-ish.
	if !strings.Contains(res.Sections[2].Content, "> A quoted insight") {
		t.Errorf("blockquote not prefixed; got %q", res.Sections[2].Content)
	}
	if !strings.Contains(res.Sections[2].Content, "- Point A") {
		t.Errorf("list items not prefixed; got %q", res.Sections[2].Content)
	}
}

func TestExtract_FallbackToBodySection_WhenNoHeadings(t *testing.T) {
	path := writeHTML(t, `<html><head><title>Doc</title></head><body>
<p>Just one paragraph.</p>
<p>And another.</p>
</body></html>`)
	res, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1", len(res.Sections))
	}
	if res.Sections[0].Title != "Doc" {
		t.Errorf("Title fallback = %q; want \"Doc\" (the document title)", res.Sections[0].Title)
	}
}

func TestExtract_FallbackToFilenameTitle(t *testing.T) {
	// Document with no <title> — extractor falls back to the
	// operator-visible filename, extension stripped.
	path := writeHTML(t, `<html><body><p>body</p></body></html>`)
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     path,
		OriginalName: "weekly-update.html",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Title != "weekly-update" {
		t.Errorf("Title fallback = %q; want \"weekly-update\"", res.Metadata.Title)
	}
}

func TestExtract_NoBodyContent_Errors(t *testing.T) {
	// html.Parse always synthesizes a <body> even when the input
	// omits one; we surface the empty-body case via the
	// "no extractable text" branch instead. Both error paths
	// give the operator the same actionable signal.
	path := writeHTML(t, `<html><head><title>x</title></head></html>`)
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Errorf("expected no-extractable-text error; got %v", err)
	}
}

func TestExtract_BodyButNoText_Errors(t *testing.T) {
	// HTML with <body> but only stripped boilerplate — no
	// extractable text. Should error so the operator gets a
	// clear failure instead of an empty extracted_documents row.
	path := writeHTML(t, `<html><body><script>foo</script><style>bar</style></body></html>`)
	_, err := New().Extract(context.Background(), extractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Errorf("expected no-text error; got %v", err)
	}
}

func TestSectionIDFromTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Introduction", "001-introduction"},
		{"Methods & Materials", "001-methods-materials"},
		{"Section #1: Overview", "001-section-1-overview"},
		{"", "section-001"},
		{"???!!!", "section-001"},
		// Trimmed at 40 chars to keep filenames sane.
		{strings.Repeat("x", 200), "001-" + strings.Repeat("x", 40)},
	}
	for i, c := range cases {
		got := sectionIDFromTitle(c.in, 1)
		if got != c.want {
			t.Errorf("case %d: sectionIDFromTitle(%q) = %q; want %q", i, c.in, got, c.want)
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
