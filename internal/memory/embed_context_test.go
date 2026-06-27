package memory

import (
	"strings"
	"testing"
)

func TestFirstHeading(t *testing.T) {
	cases := map[string]string{
		"# Top-level\nbody":            "Top-level",
		"intro\n\n## Subsection\nbody": "Subsection",
		"no headings here":             "",
		"":                             "",
		"# \n## \nbody":                "",
		"## first H2 wins\n# later H1": "first H2 wins",
		"plenty\nof\nfluff\nbefore\n# heading at line 5": "heading at line 5",
	}
	for in, want := range cases {
		if got := firstHeading(in); got != want {
			t.Errorf("firstHeading(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstHeading_BoundedScanWindow(t *testing.T) {
	// 30 blank lines then a heading — should NOT be found (scan window is 20).
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("\n")
	}
	b.WriteString("# never reached")
	if got := firstHeading(b.String()); got != "" {
		t.Fatalf("expected '' beyond scan window, got %q", got)
	}
}

func TestBuildEmbedContext(t *testing.T) {
	// Both source and section present.
	got := buildEmbedContext("research/old.md", "# Deploy Script\nbody")
	if !strings.HasPrefix(got, "Source: research/old.md\n") || !strings.Contains(got, "Section: Deploy Script\n") {
		t.Fatalf("missing labels: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("prefix should end with blank line separator: %q", got)
	}
	// Source only.
	got = buildEmbedContext("notes.md", "just body, no heading")
	if !strings.Contains(got, "Source: notes.md") || strings.Contains(got, "Section:") {
		t.Fatalf("source-only: %q", got)
	}
	// Section only.
	got = buildEmbedContext("", "# Only Heading\nbody")
	if strings.Contains(got, "Source:") || !strings.Contains(got, "Section: Only Heading") {
		t.Fatalf("section-only: %q", got)
	}
	// Neither — empty string so the embed input equals raw content.
	if got := buildEmbedContext("", "body with nothing"); got != "" {
		t.Fatalf("expected empty when no signal: %q", got)
	}
	// Whitespace-only source treated as empty.
	if got := buildEmbedContext("   ", "body"); got != "" {
		t.Fatalf("whitespace source: %q", got)
	}
}

func TestApplyEmbedContext(t *testing.T) {
	out := applyEmbedContext("doc.md", "# Title\nbody text")
	if !strings.Contains(out, "Source: doc.md") || !strings.HasSuffix(out, "# Title\nbody text") {
		t.Fatalf("applied form: %q", out)
	}
	// No-context path returns the original content unchanged.
	raw := "no context to add"
	if got := applyEmbedContext("", raw); got != raw {
		t.Fatalf("expected pass-through, got %q", got)
	}
}
