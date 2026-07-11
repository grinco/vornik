package config

import (
	"errors"
	"strings"
	"testing"
)

// EditFrontmatter (LLD 2026-07-11 actionable-proposals §4.2): apply a YAML
// edit to a markdown document's leading frontmatter, leaving the body
// byte-identical.

const fmFixture = `---
# workflow frontmatter
steps:
  - id: implement
    timeout: "10m"
---

# Dev pipeline

Body text with --- inside that must NOT be treated as a fence.
`

func TestEditFrontmatter_EditsOnlyTheFence(t *testing.T) {
	out, err := EditFrontmatter([]byte(fmFixture), func(fm []byte) ([]byte, error) {
		return SetYAMLListItemField(fm, "steps", "id", "implement", "timeout", "24m")
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "24m") {
		t.Fatal("frontmatter edit not applied")
	}
	if !strings.Contains(s, "# workflow frontmatter") {
		t.Fatal("frontmatter comments must be preserved")
	}
	wantBody := "# Dev pipeline\n\nBody text with --- inside that must NOT be treated as a fence.\n"
	if !strings.HasSuffix(s, wantBody) {
		t.Fatalf("body must be byte-identical; got tail: %q", s[len(s)-min(len(s), 120):])
	}
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("output must keep the opening fence")
	}
	if strings.Count(s, "\n---\n") < 1 {
		t.Fatal("output must keep the closing fence")
	}
}

func TestEditFrontmatter_NoFenceErrors(t *testing.T) {
	_, err := EditFrontmatter([]byte("just markdown, no fence\n"), func(_ []byte) ([]byte, error) { return nil, nil })
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Fatalf("want ErrNoFrontmatter, got %v", err)
	}
	// Fence opened but never closed.
	_, err = EditFrontmatter([]byte("---\nsteps: []\nno closing fence\n"), func(_ []byte) ([]byte, error) { return nil, nil })
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Fatalf("unterminated fence: want ErrNoFrontmatter, got %v", err)
	}
}

func TestEditFrontmatter_EditErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	_, err := EditFrontmatter([]byte(fmFixture), func(_ []byte) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("edit error must propagate, got %v", err)
	}
}

func TestEditFrontmatter_ToleratesLeadingWhitespace(t *testing.T) {
	// The registry's splitFrontmatter tolerates a BOM / leading whitespace
	// before the opening fence — EditFrontmatter must accept the same
	// documents the daemon loads, and preserve the prefix bytes on output.
	in := []byte("\n---\nsteps: []\n---\nbody\n")
	out, err := EditFrontmatter(in, func(fm []byte) ([]byte, error) {
		out, _, err := SetYAMLKey(fm, "steps", "kept")
		return out, err
	})
	if err != nil {
		t.Fatalf("leading blank line must be tolerated (registry parity): %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "\n---\n") {
		t.Fatalf("leading prefix bytes must be preserved, got %q", s[:min(len(s), 10)])
	}
	if !strings.HasSuffix(s, "body\n") {
		t.Fatal("body must be preserved")
	}
}
