package docsmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFrontmatter_ExtractsSources(t *testing.T) {
	md := []byte("---\nsources:\n  - path: docs/x.md\n    sha256: abc123\n---\n# Title\n\nbody\n")
	fm, had, err := ParseFrontmatter(md)
	if err != nil {
		t.Fatal(err)
	}
	if !had {
		t.Fatal("expected front-matter to be detected")
	}
	if len(fm.Sources) != 1 || fm.Sources[0].Path != "docs/x.md" || fm.Sources[0].SHA256 != "abc123" {
		t.Fatalf("unexpected sources: %+v", fm.Sources)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	_, had, err := ParseFrontmatter([]byte("# Title\n\nno front-matter\n"))
	if err != nil {
		t.Fatal(err)
	}
	if had {
		t.Fatal("expected no front-matter")
	}
}

func TestHashPath_FileChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "src.md")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := HashPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("hash must change when file content changes")
	}
}

func TestHashPath_DirectoryComposite(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashPath(sub)
	if err != nil {
		t.Fatal(err)
	}
	// Adding a file must change the composite hash.
	if err := os.WriteFile(filepath.Join(sub, "b.md"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := HashPath(sub)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("composite hash must change when a file is added to the directory")
	}
}

func TestStaleSources_DetectsMismatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "src.md"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "page.md")
	// Anchored hash is wrong on purpose => stale.
	if err := os.WriteFile(page, []byte("---\nsources:\n  - path: src.md\n    sha256: deadbeef\n---\n# p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := StaleSources(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "src.md" {
		t.Fatalf("expected src.md stale; got %v", stale)
	}
}

func TestStaleSources_NoAnchorSkips(t *testing.T) {
	root := t.TempDir()
	page := filepath.Join(root, "page.md")
	if err := os.WriteFile(page, []byte("# no front-matter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := StaleSources(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("unanchored page must be skipped; got %v", stale)
	}
}

func TestRestamp_FixesHashAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "src.md"), []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(root, "page.md")
	if err := os.WriteFile(page, []byte("---\nsources:\n  - path: src.md\n    sha256: stale\n---\n# p\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("restamp must change a stale page")
	}
	// After restamp the page must be fresh.
	stale, err := StaleSources(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("page must be fresh after restamp; got %v", stale)
	}
	// Body must survive.
	b, _ := os.ReadFile(page)
	if !contains(string(b), "body") {
		t.Fatalf("body lost after restamp: %s", b)
	}
	// Idempotent: second restamp changes nothing.
	changed2, err := Restamp(root, page)
	if err != nil {
		t.Fatal(err)
	}
	if changed2 {
		t.Fatal("second restamp must be a no-op")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestForbiddenHits_CatchesInternalMarkers(t *testing.T) {
	// The repo-URL hit is sourced from the denylist param ("example-private"),
	// not a hardcoded pattern — operator-identity tokens never live in shipped
	// detector source (see TestForbiddenPatterns_NoHardcodedOperatorIdentity).
	text := "Use the foo command.\nSee internal/memory/class.go for details.\nintroduced by migration 75\nclone github.com/example-private/legacy\nnormal customer line\n"
	hits := ForbiddenHits(text, []string{"example-private"})
	if len(hits) < 3 {
		t.Fatalf("expected to catch internal path, migration ref, and denylisted repo URL; got %v", hits)
	}
}

// TestForbiddenPatterns_NoHardcodedOperatorIdentity guards the CE-export IP-leak
// fix (2026-06-27): docsmeta.go ships to the public CE (cmd/docs-gen depends on
// it), so its built-in patterns must carry NO operator-identity / old-brand
// token. Those belong ONLY in the pruned scripts/docs-ip-denylist.txt and reach
// the detector via the denylist param. The forbidden stems are DERIVED from
// that denylist (not hardcoded here) so this guard test itself ships no
// operator token. If the denylist is absent (e.g. a pruned CE checkout) the
// test skips — the exported patterns were already proven clean at export time.
func TestForbiddenPatterns_NoHardcodedOperatorIdentity(t *testing.T) {
	deny, err := LoadDenylist(filepath.Join("..", "..", "scripts", "docs-ip-denylist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deny) == 0 {
		t.Skip("denylist absent (pruned CE checkout) — patterns already exported clean")
	}
	// Leading run of ASCII letters of each token, e.g. "foo.example"->"foo",
	// "bar_baz"->"bar"; a token starting with a non-letter ("@x") yields "".
	var stems []string
	for _, tok := range deny {
		s := strings.ToLower(tok)
		i := 0
		for i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
			i++
		}
		if stem := s[:i]; len(stem) >= 4 {
			stems = append(stems, stem)
		}
	}
	for _, re := range forbiddenPatterns {
		p := strings.ToLower(re.String())
		for _, stem := range stems {
			if strings.Contains(p, stem) {
				t.Errorf("pattern /%s/ holds denylisted stem %q — move it to the denylist file", re.String(), stem)
			}
		}
	}
}

func TestForbiddenHits_AllowsCleanText(t *testing.T) {
	text := "# Configuration\n\nSet `api.auth_enabled` to require a key.\nRun `swarmctl task submit`.\n"
	if hits := ForbiddenHits(text, nil); len(hits) != 0 {
		t.Fatalf("clean customer text must have no hits; got %v", hits)
	}
}

func TestForbiddenHits_DenylistSubstringCaseInsensitive(t *testing.T) {
	text := "deploy on Companion-Example project\n"
	if hits := ForbiddenHits(text, []string{"companion-example"}); len(hits) == 0 {
		t.Fatal("denylist substring must match case-insensitively")
	}
}
