package docsmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestamp_PreservesUnrelatedFrontmatterKeys pins a data-loss bug found while
// extending the anchor mechanism from docs/public to the LLD corpus
// (docs/low-level-design/2026-08-06-lld-drift-detection-design.md check A).
//
// Restamp unmarshalled front matter into Frontmatter{Sources}, then marshalled
// THAT struct back over the file — so every key it does not know about was
// silently deleted. The package doc acknowledged this by requiring public pages
// to carry `sources:` and nothing else.
//
// That constraint cannot hold for LLDs: check B keys on a `delivers:` block, so
// an anchored LLD carries both. Stamping one would have thrown `delivers:` away
// and, with it, check B's coverage for that doc — a silent regression in the
// tooling meant to detect silent regressions.
func TestRestamp_PreservesUnrelatedFrontmatterKeys(t *testing.T) {
	root := t.TempDir()
	srcPath := filepath.Join(root, "target.go")
	if err := os.WriteFile(srcPath, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	doc := filepath.Join(root, "design.md")
	original := `---
delivers:
  - internal/foo/bar.go
  - cmd/baz/main.go
sources:
  - path: target.go
    sha256: deadbeef
---
# Design

Body text.
`
	if err := os.WriteFile(doc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := Restamp(root, doc)
	if err != nil {
		t.Fatalf("Restamp: %v", err)
	}
	if !changed {
		t.Fatal("expected Restamp to rewrite the stale hash")
	}

	out, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if !strings.Contains(got, "delivers:") {
		t.Errorf("Restamp DROPPED the delivers: block — front matter it does not model must survive:\n%s", got)
	}
	for _, want := range []string{"internal/foo/bar.go", "cmd/baz/main.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("Restamp lost delivers entry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "deadbeef") {
		t.Errorf("the stale hash was not replaced:\n%s", got)
	}
	if !strings.Contains(got, "# Design") || !strings.Contains(got, "Body text.") {
		t.Errorf("body was damaged:\n%s", got)
	}

	// Idempotence: a second run must be a no-op, or `make lint` would report an
	// endless diff.
	changed2, err := Restamp(root, doc)
	if err != nil {
		t.Fatalf("second Restamp: %v", err)
	}
	if changed2 {
		t.Error("Restamp is not idempotent — a second run rewrote the file again")
	}
}

// TestRestamp_NoSourcesLeavesFileAlone guards the case where a doc carries
// delivers: but no sources: — most LLDs, since anchoring is ratcheted in.
func TestRestamp_NoSourcesLeavesFileAlone(t *testing.T) {
	root := t.TempDir()
	doc := filepath.Join(root, "design.md")
	original := "---\ndelivers:\n  - internal/foo/bar.go\n---\n# D\n"
	if err := os.WriteFile(doc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Restamp(root, doc)
	if err != nil {
		t.Fatalf("Restamp: %v", err)
	}
	if changed {
		t.Error("a doc with no sources: must not be rewritten")
	}
	out, _ := os.ReadFile(doc)
	if string(out) != original {
		t.Errorf("file modified:\n%s", out)
	}
}
