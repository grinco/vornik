package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// briefServer wires up a Server with the form-fixture project +
// a reloader that actually reloads the registry, so tests can
// assert on the in-memory project.Brief after save.
func briefServer(t *testing.T, root string) (*Server, *reloadingReloader) {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))
	return server, reloader
}

func postBriefForm(values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/projects/form-demo/brief", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// TestProjectBriefEdit_NoBriefYet — GET against a project with
// no PROJECT.md companion still renders the editor (the "create
// brief" flow). All section textareas start empty.
func TestProjectBriefEdit_NoBriefYet(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := briefServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/brief", nil)
	rec := httptest.NewRecorder()
	server.ProjectBriefEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "form-demo", "projectId should render somewhere on the page")
	assert.Contains(t, body, "name=\"goal\"", "Goal textarea must be present even when brief is empty")
}

// TestProjectBriefEdit_PopulatesFromExistingBrief — GET against
// a project that already has a PROJECT.md surfaces each field's
// current value as the initial form state.
func TestProjectBriefEdit_PopulatesFromExistingBrief(t *testing.T) {
	root := writeFormFixture(t)
	briefMd := `---
projectId: form-demo
displayName: Briefed Demo
---

Hero preamble paragraph.

## Goal

Existing goal text.

## Audience

Operators reviewing.

## Success criteria

Renders cleanly.
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "form-demo.md"), []byte(briefMd), 0o644))
	server, _ := briefServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/brief", nil)
	rec := httptest.NewRecorder()
	server.ProjectBriefEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Briefed Demo", "frontmatter displayName must round-trip into the form")
	assert.Contains(t, body, "Hero preamble paragraph.", "preamble must round-trip into the description field")
	assert.Contains(t, body, "Existing goal text.", "Goal section must round-trip")
	assert.Contains(t, body, "Operators reviewing.", "Audience section must round-trip")
}

// TestProjectBriefEdit_InvalidProjectID — slash-traversal in
// projectId is rejected the same way the form editor rejects it.
func TestProjectBriefEdit_InvalidProjectID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := briefServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/etc/brief", nil)
	rec := httptest.NewRecorder()
	server.ProjectBriefEdit(rec, req, "../etc/passwd")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestProjectBriefEdit_UnknownProjectID — GET against a project
// that doesn't exist in the registry returns 404; we don't want
// to scaffold a brief for a project whose ID is a typo.
func TestProjectBriefEdit_UnknownProjectID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := briefServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/no-such/brief", nil)
	rec := httptest.NewRecorder()
	server.ProjectBriefEdit(rec, req, "no-such")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestProjectBriefSave_CreatesNewBrief — when no PROJECT.md
// exists, the POST handler writes a fresh file, the registry
// reloads, the in-memory Project.Brief is populated, and the
// form-editor side of the integration sees HasBrief=true on the
// next GET.
func TestProjectBriefSave_CreatesNewBrief(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := briefServer(t, root)
	briefPath := filepath.Join(root, "projects", "form-demo.md")
	_, statErr := os.Stat(briefPath)
	require.True(t, os.IsNotExist(statErr), "fixture must not start with a PROJECT.md")

	form := url.Values{}
	form.Set("displayName", "Brief Created")
	form.Set("description", "Hero paragraph.")
	form.Set("goal", "Process inbound chat requests.")
	form.Set("audience", "Operators.")
	form.Set("successCriteria", "Replies cite sources.")
	form.Set("outOfScope", "Trading.")
	form.Set("riskCadence", "Low-risk.")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)
	written, err := os.ReadFile(briefPath)
	require.NoError(t, err)
	got := string(written)
	assert.Contains(t, got, "projectId: \"form-demo\"")
	assert.Contains(t, got, "## Goal\n\nProcess inbound chat requests.")
	// Round-trip through registry: brief attached, displayName
	// override from frontmatter wins.
	server2, _ := briefServer(t, root)
	proj := server2.projectReg.GetProject("form-demo")
	require.NotNil(t, proj)
	require.NotNil(t, proj.Brief, "registry reload should populate Project.Brief")
	assert.Equal(t, "Brief Created", proj.DisplayName, "brief displayName should override yaml on load")
}

// TestProjectBriefSave_UpdatesExistingBrief — when PROJECT.md
// already exists, save overwrites it AND preserves the
// timestamped backup of the prior content. Mirrors the YAML
// editor's safety net.
func TestProjectBriefSave_UpdatesExistingBrief(t *testing.T) {
	root := writeFormFixture(t)
	briefPath := filepath.Join(root, "projects", "form-demo.md")
	prior := []byte(`---
projectId: form-demo
---

## Goal

original goal

## Audience

original audience

## Success criteria

original success

## References

ref body
`)
	require.NoError(t, os.WriteFile(briefPath, prior, 0o600))
	server, reloader := briefServer(t, root)

	form := url.Values{}
	form.Set("goal", "updated goal")
	form.Set("audience", "updated audience")
	form.Set("successCriteria", "updated success")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	got, err := os.ReadFile(briefPath)
	require.NoError(t, err)
	body := string(got)
	assert.Contains(t, body, "updated goal")
	assert.NotContains(t, body, "original goal")
	assert.Contains(t, body, "## References", "Extra section from prior content must survive the save")
	assert.Contains(t, body, "ref body")

	backups, err := filepath.Glob(briefPath + ".bak-*")
	require.NoError(t, err)
	require.Len(t, backups, 1, "prior brief content must be backed up")
	backup, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(prior), string(backup))
}

// TestProjectBriefSave_MissingRequiredSectionRejected — a save
// that omits Goal / Audience / Success criteria returns 400 and
// does NOT write to disk. Matches the parser's required-section
// contract from Phase 1A.
func TestProjectBriefSave_MissingRequiredSectionRejected(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := briefServer(t, root)
	briefPath := filepath.Join(root, "projects", "form-demo.md")

	form := url.Values{}
	form.Set("goal", "")
	form.Set("audience", "ops")
	form.Set("successCriteria", "renders")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "form-demo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Goal")
	assert.Equal(t, 0, reloader.calls)
	_, statErr := os.Stat(briefPath)
	assert.True(t, os.IsNotExist(statErr), "no PROJECT.md should be written on validation failure")
}

// TestProjectBriefSave_InvalidProjectID — slash-traversal is
// rejected at the handler boundary.
func TestProjectBriefSave_InvalidProjectID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := briefServer(t, root)

	form := url.Values{}
	form.Set("goal", "g")
	form.Set("audience", "a")
	form.Set("successCriteria", "s")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "../etc/passwd")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTrimParserPrefix — covers both branches: messages with
// the "PROJECT.md <file>:" prefix get trimmed; messages without
// it pass through untouched. Used by the brief handler to keep
// the inline error banner focused on the reason rather than the
// file plumbing.
func TestTrimParserPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"PROJECT.md foo.md: required brief section '## Goal' is missing", "required brief section '## Goal' is missing"},
		{"something else entirely", "something else entirely"},
		{"PROJECT.md", "PROJECT.md"}, // no ": " separator → pass-through
	}
	for _, tc := range cases {
		got := trimParserPrefix(tc.in)
		assert.Equal(t, tc.want, got, "trimParserPrefix(%q)", tc.in)
	}
}

// TestProjectBriefSave_ProjectRegFallback — when no
// ConfigReloader is wired, the brief save falls back to a
// direct projectReg.Load. Covers the alternate reload branch
// the form-config save also has.
func TestProjectBriefSave_ProjectRegFallback(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg)) // no WithConfigReloader

	form := url.Values{}
	form.Set("goal", "fallback g")
	form.Set("audience", "a")
	form.Set("successCriteria", "s")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	require.NotNil(t, proj.Brief, "fallback reload should populate Project.Brief")
	assert.Contains(t, proj.Brief.Goal, "fallback g")
}

// TestProjectBriefSave_ReloadErrorReportsConflict — file
// written but reload failed: operator sees 409 and the backup
// path so they can roll back.
func TestProjectBriefSave_ReloadErrorReportsConflict(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &erroringReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	form := url.Values{}
	form.Set("goal", "g")
	form.Set("audience", "a")
	form.Set("successCriteria", "s")

	rec := httptest.NewRecorder()
	server.ProjectBriefSave(rec, postBriefForm(form), "form-demo")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 1, reloader.calls)
}
