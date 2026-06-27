package docsmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- LoadDenylist (was 0% covered) ---

func TestW3DocsLoadDenylist_PresentStripsCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deny.txt")
	content := "# a comment\n\ncompanion-example\n   example-corp   \n# another\n\nsecret-project\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDenylist(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"companion-example", "example-corp", "secret-project"}
	if len(got) != len(want) {
		t.Fatalf("expected %d entries, got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: want %q got %q (all: %v)", i, want[i], got[i], got)
		}
	}
}

func TestW3DocsLoadDenylist_MissingFileNoError(t *testing.T) {
	got, err := LoadDenylist(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got != nil {
		t.Fatalf("missing file must yield nil list, got %v", got)
	}
}

func TestW3DocsLoadDenylist_EmptyFileYieldsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deny.txt")
	if err := os.WriteFile(p, []byte("\n\n#only a comment\n   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDenylist(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("all-comment/blank file must yield empty list, got %v", got)
	}
}

func TestW3DocsLoadDenylist_DirAsPathErrors(t *testing.T) {
	// A directory exists (not IsNotExist) so the read must surface an error.
	dir := t.TempDir()
	_, err := LoadDenylist(dir)
	if err == nil {
		t.Fatal("opening a directory as a denylist file must error")
	}
}

// --- SplitFrontmatter edge cases ---

func TestW3DocsSplitFrontmatter_ValidFences(t *testing.T) {
	md := []byte("---\nsources:\n  - path: a.md\n    sha256: x\n---\n# Title\n\nbody text\n")
	fmBytes, body, had := SplitFrontmatter(md)
	if !had {
		t.Fatal("expected front-matter to be detected")
	}
	if !strings.Contains(string(fmBytes), "sources:") {
		t.Fatalf("front-matter block lost: %q", fmBytes)
	}
	if strings.Contains(string(fmBytes), "Title") {
		t.Fatalf("body leaked into front-matter: %q", fmBytes)
	}
	if !strings.Contains(string(body), "# Title") || !strings.Contains(string(body), "body text") {
		t.Fatalf("body not preserved: %q", body)
	}
}

func TestW3DocsSplitFrontmatter_NoOpeningFence(t *testing.T) {
	md := []byte("# Title\n\njust a body\n")
	fmBytes, body, had := SplitFrontmatter(md)
	if had {
		t.Fatal("no opening fence => had must be false")
	}
	if fmBytes != nil {
		t.Fatalf("fmBytes must be nil, got %q", fmBytes)
	}
	if string(body) != string(md) {
		t.Fatalf("body must equal input when unfenced, got %q", body)
	}
}

func TestW3DocsSplitFrontmatter_MissingClosingFence(t *testing.T) {
	// Opens with --- but never closes => treated as no front-matter.
	md := []byte("---\nsources:\n  - path: a.md\nno closing fence here\n")
	fmBytes, body, had := SplitFrontmatter(md)
	if had {
		t.Fatal("partial fence (no close) must report had=false")
	}
	if fmBytes != nil {
		t.Fatalf("fmBytes must be nil, got %q", fmBytes)
	}
	if string(body) != string(md) {
		t.Fatalf("body must equal input on partial fence, got %q", body)
	}
}

func TestW3DocsSplitFrontmatter_EmptyInput(t *testing.T) {
	fmBytes, body, had := SplitFrontmatter([]byte(""))
	if had {
		t.Fatal("empty input must report had=false")
	}
	if fmBytes != nil {
		t.Fatalf("fmBytes must be nil on empty input, got %q", fmBytes)
	}
	if len(body) != 0 {
		t.Fatalf("body must be empty on empty input, got %q", body)
	}
}

func TestW3DocsSplitFrontmatter_EmptyFrontmatterBlock(t *testing.T) {
	// "---\n---\n..." — fence opens, and the very next line is the closing
	// fence. Per the implementation the closer is found via "\n---\n", so an
	// empty block needs a leading newline before the closing fence.
	md := []byte("---\n\n---\nbody\n")
	fmBytes, body, had := SplitFrontmatter(md)
	if !had {
		t.Fatal("expected an (empty) front-matter block to be detected")
	}
	if strings.TrimSpace(string(fmBytes)) != "" {
		t.Fatalf("front-matter block should be empty, got %q", fmBytes)
	}
	if string(body) != "body\n" {
		t.Fatalf("body must be %q, got %q", "body\n", body)
	}
}

// --- ParseFrontmatter ---

func TestW3DocsParseFrontmatter_Malformed(t *testing.T) {
	// Valid fences but the YAML inside is invalid (sources should be a list).
	md := []byte("---\nsources: : not valid yaml here :\n  - broken\n---\nbody\n")
	_, had, err := ParseFrontmatter(md)
	if err == nil {
		t.Fatal("malformed YAML in front-matter must return an error")
	}
	if !had {
		t.Fatal("had must be true even when the YAML fails to parse")
	}
}

func TestW3DocsParseFrontmatter_MissingSourcesKey(t *testing.T) {
	// Well-formed YAML but no `sources:` key => zero-value Frontmatter, no error.
	md := []byte("---\ntitle: Something\nother: 1\n---\nbody\n")
	fm, had, err := ParseFrontmatter(md)
	if err != nil {
		t.Fatalf("unrecognised keys must not error: %v", err)
	}
	if !had {
		t.Fatal("front-matter present => had must be true")
	}
	if len(fm.Sources) != 0 {
		t.Fatalf("missing sources key => empty Sources, got %v", fm.Sources)
	}
}

func TestW3DocsParseFrontmatter_MultipleSources(t *testing.T) {
	md := []byte("---\nsources:\n  - path: a.md\n    sha256: aaa\n  - path: b.md\n    sha256: bbb\n---\nbody\n")
	fm, had, err := ParseFrontmatter(md)
	if err != nil {
		t.Fatal(err)
	}
	if !had || len(fm.Sources) != 2 {
		t.Fatalf("expected 2 sources, got had=%v sources=%v", had, fm.Sources)
	}
	if fm.Sources[0].Path != "a.md" || fm.Sources[1].SHA256 != "bbb" {
		t.Fatalf("sources parsed wrong: %+v", fm.Sources)
	}
}

// --- HashPath / hashFile ---

func TestW3DocsHashPath_Stability(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "src.md")
	if err := os.WriteFile(p, []byte("stable content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashPath(p)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash must be stable for unchanged content: %q vs %q", h1, h2)
	}
	// sha256 hex is 64 chars.
	if len(h1) != 64 {
		t.Fatalf("expected 64-char hex sha256, got %d chars: %q", len(h1), h1)
	}
}

func TestW3DocsHashPath_KnownVector(t *testing.T) {
	// sha256("") = e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
	dir := t.TempDir()
	p := filepath.Join(dir, "empty")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := HashPath(p)
	if err != nil {
		t.Fatal(err)
	}
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != want {
		t.Fatalf("empty-file sha256 mismatch: want %s got %s", want, h)
	}
}

func TestW3DocsHashPath_MissingFileErrors(t *testing.T) {
	_, err := HashPath(filepath.Join(t.TempDir(), "nope.md"))
	if err == nil {
		t.Fatal("HashPath on a missing path must error (os.Stat fails)")
	}
}

func TestW3DocsHashPath_DirContentSensitiveNotJustNames(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "tree")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "a.md")
	if err := os.WriteFile(f, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashPath(sub)
	if err != nil {
		t.Fatal(err)
	}
	// Same filename, different content => composite hash must change.
	if err := os.WriteFile(f, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := HashPath(sub)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("composite dir hash must change when a file's content changes")
	}
}

// --- StaleSources ---

func TestW3DocsStaleSources_FreshWhenHashMatches(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.md")
	if err := os.WriteFile(src, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := HashPath(src)
	if err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "page.md")
	body := "---\nsources:\n  - path: src.md\n    sha256: " + h + "\n---\n# p\n"
	if err := os.WriteFile(page, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := StaleSources(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("matching hash must be fresh, got %v", stale)
	}
}

func TestW3DocsStaleSources_MissingPageErrors(t *testing.T) {
	_, err := StaleSources(t.TempDir(), filepath.Join(t.TempDir(), "ghost.md"))
	if err == nil {
		t.Fatal("StaleSources on a missing page must error")
	}
}

func TestW3DocsStaleSources_MissingSourceErrors(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	// Anchors a source path that does not exist under root.
	if err := os.WriteFile(page, []byte("---\nsources:\n  - path: gone.md\n    sha256: x\n---\n# p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := StaleSources(root, page)
	if err == nil {
		t.Fatal("StaleSources must error when an anchored source is missing")
	}
	if !strings.Contains(err.Error(), "gone.md") {
		t.Fatalf("error should name the missing source, got %v", err)
	}
}

func TestW3DocsStaleSources_MalformedFrontmatterErrors(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	if err := os.WriteFile(page, []byte("---\nsources: : bad : yaml\n  - x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := StaleSources(root, page)
	if err == nil {
		t.Fatal("malformed front-matter must propagate a parse error")
	}
}

// --- Restamp ---

func TestW3DocsRestamp_NoFrontmatterNoChange(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	orig := "# Plain page\n\nno front-matter\n"
	if err := os.WriteFile(page, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("page without front-matter must not be changed")
	}
	got, _ := os.ReadFile(page)
	if string(got) != orig {
		t.Fatalf("file must be untouched, got %q", got)
	}
}

func TestW3DocsRestamp_EmptySourcesNoChange(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	// Front-matter present but no sources => left untouched.
	orig := "---\ntitle: x\n---\nbody\n"
	if err := os.WriteFile(page, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("page with empty sources must not be changed")
	}
	got, _ := os.ReadFile(page)
	if string(got) != orig {
		t.Fatalf("file must be untouched, got %q", got)
	}
}

func TestW3DocsRestamp_WritesCurrentHashToDisk(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.md")
	if err := os.WriteFile(src, []byte("real content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := HashPath(src)
	if err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "page.md")
	if err := os.WriteFile(page, []byte("---\nsources:\n  - path: src.md\n    sha256: wrong\n---\n# title\n\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("restamp of a stale page must report changed")
	}
	// Re-read and re-parse the on-disk result.
	b, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	fm, had, err := ParseFrontmatter(b)
	if err != nil || !had {
		t.Fatalf("restamped page must still parse: had=%v err=%v", had, err)
	}
	if len(fm.Sources) != 1 || fm.Sources[0].SHA256 != want {
		t.Fatalf("on-disk sha256 not updated to current hash: want %s got %+v", want, fm.Sources)
	}
	if !strings.Contains(string(b), "keep me") {
		t.Fatalf("body must survive restamp, got %q", b)
	}
}

func TestW3DocsRestamp_MissingFileErrors(t *testing.T) {
	_, err := Restamp(t.TempDir(), filepath.Join(t.TempDir(), "nope.md"))
	if err == nil {
		t.Fatal("Restamp on a missing file must error")
	}
}

func TestW3DocsRestamp_MissingSourceErrors(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	if err := os.WriteFile(page, []byte("---\nsources:\n  - path: absent.md\n    sha256: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Restamp(root, page)
	if err == nil {
		t.Fatal("Restamp must error when an anchored source is missing")
	}
	if !strings.Contains(err.Error(), "absent.md") {
		t.Fatalf("error should name the missing source, got %v", err)
	}
}

func TestW3DocsRestamp_AlreadyFreshNoChange(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.md")
	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := HashPath(src)
	if err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "page.md")
	fresh := "---\nsources:\n    - path: src.md\n      sha256: " + h + "\n---\nbody\n"
	if err := os.WriteFile(page, []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}
	// First restamp may re-marshal whitespace (indent differs) => could change,
	// but the second must be a stable no-op.
	if _, err := Restamp(root, page); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("restamp of an already-canonical fresh page must be a no-op")
	}
}
