package ui

import (
	"bytes"
	"strings"
	"testing"
)

// assertNoLegacyTokens fails if templateFile still references any legacy
// gray-*/dark-* Tailwind class — the Phase-0 token migration removed them in
// favour of semantic ink-*/surface-* tokens. Source-scoped (reads the embedded
// template directly) so it's unaffected by shared partials rendered into the body.
func assertNoLegacyTokens(t *testing.T, templateFile string) {
	t.Helper()
	src, err := templatesFS.ReadFile("templates/" + templateFile)
	if err != nil {
		t.Fatalf("read %s: %v", templateFile, err)
	}
	// "gray-" catches every property (text-/bg-/ring-/border-/divide-gray-);
	// "dark-" catches the whole legacy dark surface scale on any property
	// and any shade, without matching the `[data-theme="dark"]` selector
	// (which is `dark"]`, not `dark-`).
	for _, legacy := range []string{"gray-", "dark-"} {
		if strings.Contains(string(src), legacy) {
			t.Errorf("%s still contains legacy token %q", templateFile, legacy)
		}
	}
}

func renderPartial(t *testing.T, name string, data any) string {
	t.Helper()
	srv := NewServer()
	var buf bytes.Buffer
	if err := srv.templates.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("ExecuteTemplate(%q): %v", name, err)
	}
	return buf.String()
}

func TestSectionHeaderPartial(t *testing.T) {
	out := renderPartial(t, "sectionHeader", map[string]any{
		"Title": "Conversation", "Href": "/ui/x", "HrefLabel": "drill in →",
	})
	for _, want := range []string{"Conversation", "/ui/x", "drill in →", "text-ink-200"} {
		if !strings.Contains(out, want) {
			t.Errorf("sectionHeader missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "text-gray-") || strings.Contains(out, "border-dark-") {
		t.Errorf("sectionHeader emitted legacy tokens:\n%s", out)
	}
}

func TestSectionHeaderMetaTextPath(t *testing.T) {
	// MetaText branch: muted span, NO anchor.
	out := renderPartial(t, "sectionHeader", map[string]any{
		"Title": "Lease", "MetaText": "expires in 4m",
	})
	if !strings.Contains(out, "expires in 4m") {
		t.Errorf("sectionHeader missing MetaText:\n%s", out)
	}
	if strings.Contains(out, "<a ") {
		t.Errorf("sectionHeader rendered an anchor without HrefLabel:\n%s", out)
	}
}

func TestSectionHeaderBareTitle(t *testing.T) {
	// All optionals absent — renders without error, no anchor/span.
	out := renderPartial(t, "sectionHeader", map[string]any{"Title": "Phases"})
	if !strings.Contains(out, "Phases") {
		t.Errorf("sectionHeader missing bare Title:\n%s", out)
	}
	if strings.Contains(out, "<a ") {
		t.Errorf("sectionHeader rendered an anchor for a bare title:\n%s", out)
	}
}

func TestStatStripPartial(t *testing.T) {
	out := renderPartial(t, "statStrip", map[string]any{
		"Items": []map[string]any{
			{"Label": "Status", "Value": "RUNNING"},
			{"Label": "Attempt", "Value": "2", "Mono": true},
		},
	})
	for _, want := range []string{"Status", "RUNNING", "Attempt", "font-mono", "text-ink-500"} {
		if !strings.Contains(out, want) {
			t.Errorf("statStrip missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "text-gray-") || strings.Contains(out, "border-dark-") {
		t.Errorf("statStrip emitted legacy tokens:\n%s", out)
	}
}

// TestAllPagesNoLegacyTokens sweeps all four re-tiered templates and
// asserts none contain legacy gray-*/dark-* Tailwind tokens. This is
// the definitive guard against theme-toggle regressions introduced by
// future edits — a single test covers all four pages rather than
// spreading the check across per-page files.
func TestAllPagesNoLegacyTokens(t *testing.T) {
	for _, f := range []string{
		"dashboard.html",
		"task_detail.html",
		"project_detail.html",
		"execution.html",
	} {
		t.Run(f, func(t *testing.T) {
			assertNoLegacyTokens(t, f)
		})
	}
}

// TestNoOrphanDescriptionTerms asserts that no <dt>/<dd> element appears
// outside an open <dl> in the re-tiered templates (invalid HTML; screen
// readers won't associate term↔description). Sound structural scan: walk
// the source tracking <dl> nesting depth and flag any <dt>/<dd> seen at
// depth 0. This correctly handles a single <dl> wrapping many <dt> items
// (depth stays >0) — unlike a naive count(<dl) >= count(<dt) heuristic.
func TestNoOrphanDescriptionTerms(t *testing.T) {
	for _, f := range []string{"task_detail.html", "execution.html"} {
		t.Run(f, func(t *testing.T) {
			src, err := templatesFS.ReadFile("templates/" + f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			s := string(src)
			depth := 0
			for i := 0; i < len(s); {
				switch {
				case strings.HasPrefix(s[i:], "</dl>"):
					if depth > 0 {
						depth--
					}
					i += 5
				case strings.HasPrefix(s[i:], "<dl"):
					depth++
					i += 3
				case strings.HasPrefix(s[i:], "<dt"), strings.HasPrefix(s[i:], "<dd"):
					if depth == 0 {
						t.Errorf("%s: <%s> at offset %d is outside any <dl> (orphan description term)", f, s[i+1:i+3], i)
					}
					i += 3
				default:
					i++
				}
			}
		})
	}
}

// TestSemanticColorFamiliesRegistered guards the root invariant the
// ink/surface token migration depends on: the `ink` and `surface` Tailwind
// color families MUST be registered in the pageHead tailwind.config, or the
// CDN JIT emits no CSS for text-ink-*/bg-surface-*/border-surface-* and all
// four re-tiered pages render colorless. The string-presence/absence guards
// (assertNoLegacyTokens, the partial render tests) cannot catch this — they
// never check that a utility resolves. This test does.
func TestSemanticColorFamiliesRegistered(t *testing.T) {
	head := renderPartial(t, "pageHead", map[string]any{"Title": "x"})
	for _, fam := range []struct{ key, varPrefix string }{
		{"ink", "--ink-"},
		{"surface", "--surface-"},
	} {
		if !strings.Contains(head, fam.key+": {") {
			t.Errorf("tailwind config is missing the %q color family — text-%s-*/bg-%s-* utilities won't resolve and the re-tiered pages render colorless", fam.key, fam.key, fam.key)
		}
		if !strings.Contains(head, "rgb(var("+fam.varPrefix) {
			t.Errorf("%q family not wired to %s* CSS vars", fam.key, fam.varPrefix)
		}
	}
	// The guard is only meaningful if the pages actually use these families.
	for _, f := range []string{"dashboard.html", "task_detail.html", "project_detail.html", "execution.html"} {
		src, err := templatesFS.ReadFile("templates/" + f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(src)
		if !strings.Contains(s, "-ink-") || !strings.Contains(s, "-surface-") {
			t.Errorf("%s: expected ink/surface utility usage — migration guard may be stale", f)
		}
	}
}

func TestPanelComponentClassesPresent(t *testing.T) {
	// pageHead carries the global <style> with token vars + component classes.
	out := renderPartial(t, "pageHead", map[string]any{"Title": "x"})
	// Assert the full set the CSS ships — incl. every data-tone variant and
	// the [open] rule — so a typo in any single rule is caught.
	for _, want := range []string{
		".panel-primary", ".panel-ref", ".panel-ref > summary", ".panel-ref[open]",
		`data-tone="amber"`, `data-tone="sky"`, `data-tone="rose"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pageHead missing component class %q", want)
		}
	}
}
