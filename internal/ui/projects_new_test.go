package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/templates"
)

// templateRig builds a minimal catalog + writable configsDir for
// the projects_new tests. Two templates across two domains so the
// tab strip + filter logic both exercise.
func templateRig(t *testing.T) (*Server, string) {
	t.Helper()
	tplDir := filepath.Join(t.TempDir(), "tpl")
	require.NoError(t, os.MkdirAll(filepath.Join(tplDir, "alpha"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tplDir, "beta"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "alpha", "template.yaml"), []byte(`
displayName: "Alpha Demo"
description: "First template."
domain: "general"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
files:
  - {source: project.yaml.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "alpha", "project.yaml.tmpl"),
		[]byte("projectId: {{.projectId}}\n"), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "beta", "template.yaml"), []byte(`
displayName: "Beta Demo"
description: "Second template."
domain: "research"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
files:
  - {source: out.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "beta", "out.tmpl"),
		[]byte("projectId: {{.projectId}}\n"), 0o644))

	cat, err := templates.Load(tplDir)
	require.NoError(t, err)
	configsDir := t.TempDir()
	srv := NewServer(WithProjectTemplates(cat), WithConfigsDir(configsDir))
	return srv, configsDir
}

// TestProjectsNew_GalleryRendersTemplates pins the happy gallery
// path: every loaded template surfaces, with its domain badge.
func TestProjectsNew_GalleryRendersTemplates(t *testing.T) {
	srv, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, "Alpha Demo")
	assert.Contains(t, body, "Beta Demo")
	// Both domain badges should appear in the cards.
	assert.Contains(t, body, "general")
	assert.Contains(t, body, "research")
}

// TestProjectsNew_DomainTabStrip — the multi-domain filter UI
// renders the tabs only when more than one domain exists. This
// is the 2026-05-15 external-research-inspired taxonomy addition.
func TestProjectsNew_DomainTabStrip(t *testing.T) {
	srv, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	// Tab strip uses ?domain=<d> hrefs.
	assert.Contains(t, body, "/ui/projects/new?domain=general",
		"the multi-domain catalog must render filter tabs")
	assert.Contains(t, body, "/ui/projects/new?domain=research")
}

// TestProjectsNew_DomainFilterNarrowsGrid — when ?domain=general
// is set, the research template should be excluded from the grid.
func TestProjectsNew_DomainFilterNarrowsGrid(t *testing.T) {
	srv, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new?domain=general", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	assert.Contains(t, body, "Alpha Demo")
	assert.NotContains(t, body, "Beta Demo",
		"research-domain template must NOT render in the general-domain filter view")
}

// TestProjectsNew_SlugRendersForm — when ?slug=<known> is set,
// the form view renders with the parameter inputs instead of the
// gallery grid.
func TestProjectsNew_SlugRendersForm(t *testing.T) {
	srv, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new?slug=alpha", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	assert.Contains(t, body, `name="slug" value="alpha"`,
		"form must include hidden slug input wiring the POST back to this template")
	assert.Contains(t, body, `name="p_projectId"`,
		"each declared parameter must surface as a p_<name> input")
}

// TestProjectsNew_EmptyCatalogFallsBackGracefully — without a
// wired catalog the gallery degrades to an empty state instead
// of 500'ing.
func TestProjectsNew_EmptyCatalogFallsBackGracefully(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "No template catalog installed")
}

// TestProjectsCreateFromTemplate_HappyPath POSTs the form and
// asserts files land on disk + the success view renders.
func TestProjectsCreateFromTemplate_HappyPath(t *testing.T) {
	srv, configsDir := templateRig(t)
	form := url.Values{}
	form.Set("slug", "alpha")
	form.Set("p_projectId", "myproject")

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	body := rr.Body.String()
	assert.Contains(t, body, "Project created from")
	assert.Contains(t, body, "projects/myproject.yaml")

	// File must actually exist on disk.
	written, err := os.ReadFile(filepath.Join(configsDir, "projects/myproject.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "projectId: myproject")
}

// TestProjectsCreateFromTemplate_TriggersReload pins the regression
// where a freshly created project wasn't visible in the registry until
// the daemon restarted: the handler wrote the files to disk but never
// asked the config reloader to re-read them, so GetProject kept
// returning nil and the UI showed "Project Not Found". Every other
// config-write handler reloads after writing; this one must too.
func TestProjectsCreateFromTemplate_TriggersReload(t *testing.T) {
	srv, configsDir := templateRig(t)
	reloader := &mockConfigReloader{}
	WithConfigReloader(reloader)(srv)

	form := url.Values{}
	form.Set("slug", "alpha")
	form.Set("p_projectId", "myproject")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	assert.Equal(t, 1, reloader.calls,
		"creating a project must reload config so the new project is visible without a restart")

	// Sanity: the file did land on disk.
	_, err := os.Stat(filepath.Join(configsDir, "projects/myproject.yaml"))
	require.NoError(t, err)
}

func TestProjectsCreateFromTemplate_SessionUserForbidden(t *testing.T) {
	srv, configsDir := templateRig(t)
	form := url.Values{"slug": {"alpha"}, "p_projectId": {"forbidden-project"}}
	req := sessionUserUIRequest(http.MethodPost, "/ui/projects/new", nil)
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	srv.ProjectsCreateFromTemplate(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	_, err := os.Stat(filepath.Join(configsDir, "projects/forbidden-project.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestProjectsCreateFromTemplate_ValidationFailureRepopulates
// pins the form-error round-trip: bad input keeps the user's
// values so they don't have to retype everything.
func TestProjectsCreateFromTemplate_ValidationFailureRepopulates(t *testing.T) {
	srv, _ := templateRig(t)
	form := url.Values{}
	form.Set("slug", "alpha")
	form.Set("p_projectId", "INVALID UPPERCASE WITH SPACES")

	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "validation failures re-render the form, not 4xx")

	body := rr.Body.String()
	assert.Contains(t, body, "must match pattern",
		"the templates package's pattern-mismatch error must surface to the user")
	assert.Contains(t, body, "INVALID UPPERCASE WITH SPACES",
		"the form must repopulate the operator's value so they don't have to retype")
}

// TestProjectsCreateFromTemplate_RefusesOverwrite — same target
// file already on disk should fall through to a conflict error
// rather than silently overwriting an existing project.
func TestProjectsCreateFromTemplate_RefusesOverwrite(t *testing.T) {
	srv, configsDir := templateRig(t)
	// Pre-write the target file so the second create collides.
	require.NoError(t, os.MkdirAll(filepath.Join(configsDir, "projects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configsDir, "projects/myproject.yaml"),
		[]byte("existing\n"), 0o644))

	form := url.Values{}
	form.Set("slug", "alpha")
	form.Set("p_projectId", "myproject")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "conflict re-renders the form, not 4xx (form-level error)")
	assert.Contains(t, rr.Body.String(), "already exists")

	got, err := os.ReadFile(filepath.Join(configsDir, "projects/myproject.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "existing\n", string(got), "refused overwrite must preserve the existing project file")
}

// TestProjectsCreateFromTemplate_RejectsUnknownSlug — operator
// hand-crafting a POST shouldn't be able to render an arbitrary
// path. Unknown slug → 400.
func TestProjectsCreateFromTemplate_RejectsUnknownSlug(t *testing.T) {
	srv, _ := templateRig(t)
	form := url.Values{}
	form.Set("slug", "does-not-exist")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// TestProjectsCreateFromTemplate_RequiresCatalog — without a wired
// catalog the POST must 503 rather than NPE on a nil .Get.
func TestProjectsCreateFromTemplate_RequiresCatalog(t *testing.T) {
	srv := NewServer()
	form := url.Values{}
	form.Set("slug", "alpha")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// TestProjectsCreateFromTemplate_RequiresConfigsDir — catalog wired
// but no writable configsDir => 503 so operators see the misconfig
// rather than a 500 from a failed write.
func TestProjectsCreateFromTemplate_RequiresConfigsDir(t *testing.T) {
	tplDir := filepath.Join(t.TempDir(), "tpl")
	require.NoError(t, os.MkdirAll(filepath.Join(tplDir, "alpha"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "alpha", "template.yaml"), []byte(`
displayName: "Alpha"
description: "x"
parameters:
  - {name: projectId, type: string, label: "ID", required: true}
files:
  - {source: out.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "alpha", "out.tmpl"), []byte("x"), 0o644))
	cat, err := templates.Load(tplDir)
	require.NoError(t, err)
	srv := NewServer(WithProjectTemplates(cat)) // intentionally no configsDir

	form := url.Values{}
	form.Set("slug", "alpha")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

// TestProjectsCreateFromTemplate_MissingSlug — empty slug after
// trim returns 400 with the gallery-pointer message.
func TestProjectsCreateFromTemplate_MissingSlug(t *testing.T) {
	srv, _ := templateRig(t)
	form := url.Values{}
	form.Set("slug", "   ") // whitespace only
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.ProjectsCreateFromTemplate(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "Missing slug")
}

// TestProjectsNew_UnknownSlugInQueryFallsBackToGallery — operator
// crafts /ui/projects/new?slug=garbage; the page should render the
// gallery with a helpful error rather than 500 / blank form.
func TestProjectsNew_UnknownSlugInQueryFallsBackToGallery(t *testing.T) {
	srv, _ := templateRig(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/projects/new?slug=does-not-exist", nil)
	rr := httptest.NewRecorder()
	srv.ProjectsNew(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "Alpha Demo",
		"unknown slug should fall back to the gallery grid, not a blank page")
}
