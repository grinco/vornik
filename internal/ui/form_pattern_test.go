// Regression tests pinning the canonical "form pattern" — every
// form-bearing page in the codebase uses the same red/emerald
// banner pair driven by `.Error` / `.Success`, populated via
// typed fields on the data struct. These tests stop a future
// edit from forking the pattern again (which is what `FormError`
// + `FormValues` did until 2026-05-20).
//
// Asserts on raw HTML fragments rather than full DOM parsing —
// the goal is to detect drift, not to validate semantics.
package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalErrorBanner is the exact pill shape every form uses.
// If a new page introduces its own variant the diff lands here.
// The text shade is theme-adaptive (text-red-600 in light, dark:text-red-200
// in dark) for WCAG-AA contrast on both surfaces — see _partials.html.
const canonicalErrorBanner = `<div class="rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-600 dark:text-red-200 whitespace-pre-wrap">`

// canonicalSuccessBanner is the success counterpart. Forms that
// redirect on success still emit the block conditionally so the
// pattern stays consistent (cheap, dead code on the happy path).
const canonicalSuccessBanner = `<div class="rounded-lg border border-emerald-500/30 bg-emerald-500/10 px-4 py-3 text-sm text-emerald-600 dark:text-emerald-200">`

// TestFormPattern_ProjectsNewUsesCanonicalErrorBanner — the
// projects/new POST surface was the historical deviant
// (rose-500 + .FormError). Pin that it now uses the canon so a
// future "rose looks nicer" refactor can't sneak the divergence
// back in.
func TestFormPattern_ProjectsNewUsesCanonicalErrorBanner(t *testing.T) {
	srv, _ := templateRig(t)
	form := url.Values{}
	form.Set("slug", "alpha")
	form.Set("p_projectId", "BAD UPPERCASE")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, canonicalErrorBanner) {
		t.Errorf("projects/new error pill diverged from canon; want %q in body", canonicalErrorBanner)
	}
	if strings.Contains(body, "bg-rose-500/10 border border-rose-500/40") {
		t.Error("projects/new still emits the old rose pill — canon migration regressed")
	}
	if strings.Contains(body, "FormError") {
		t.Error("projects/new template still references FormError field — should be .Error")
	}
}

// TestFormPattern_SwarmsNewHasSuccessBanner — even though
// swarms/new redirects on success today, the template emits the
// emerald block conditionally so the source shape matches every
// edit form. Pin so a future "remove dead code" pass doesn't
// strip the block + force the next form author to reinvent it.
func TestFormPattern_SwarmsNewHasSuccessBanner(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/swarms/new", nil)
	rec := httptest.NewRecorder()
	srv.SwarmsNew(rec, req)

	_ = rec.Body.String()
	// On a bare GET, the {{if .Success}} block is hidden because
	// Success is empty — that's the common case. The next render
	// below pins the source-template shape positively: with a
	// non-empty Success it must produce the emerald banner.
	// Stronger: search the template source via the same template
	// engine — render with an injected Success value and assert
	// the emerald pill appears. ServerOption-style injection isn't
	// available, so we drive it via a one-off render through the
	// templates filesystem.
	got := renderTemplateString(t, "swarms_new.html", map[string]any{
		"Title":       "New swarm",
		"CurrentPage": "swarms",
		"Success":     "saved.",
		"Form":        map[string]string{},
	})
	if !strings.Contains(got, canonicalSuccessBanner) {
		t.Errorf("swarms/new template missing canonical success banner")
	}
	if !strings.Contains(got, "saved.") {
		t.Error("swarms/new success banner did not surface injected Success value")
	}
}

// TestFormPattern_WorkflowsNewHasSuccessBanner — same posture as
// swarms/new. Both pages live in the same family and historically
// drifted in lockstep.
func TestFormPattern_WorkflowsNewHasSuccessBanner(t *testing.T) {
	got := renderTemplateString(t, "workflows_new.html", map[string]any{
		"Title":       "New workflow",
		"CurrentPage": "workflows",
		"Success":     "saved.",
		"Form":        map[string]string{},
	})
	if !strings.Contains(got, canonicalSuccessBanner) {
		t.Errorf("workflows/new template missing canonical success banner")
	}
}

// renderTemplateString renders the named template through the
// production template registry with the supplied data. Helper
// keeps form-pattern tests focused on the canonical shape rather
// than reinventing the Server-handler plumbing for each one.
func renderTemplateString(t *testing.T, name string, data any) string {
	t.Helper()
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.render(rec, name, data)
	return rec.Body.String()
}

// TestFormPattern_NoFormErrorIdentifierLingers — defensive: the
// rename from FormError → Error should leave no stale references
// either in templates or in the embedded copies the test fixtures
// rely on. Walk the templates dir and flag any survivor.
func TestFormPattern_NoFormErrorIdentifierLingers(t *testing.T) {
	root := filepath.Join("templates")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("templates dir not readable from test cwd: %v", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		path := filepath.Join(root, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(b), ".FormError") {
			t.Errorf("%s still references .FormError — should be .Error per the form pattern canon", e.Name())
		}
	}
}
