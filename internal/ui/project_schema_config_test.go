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
	"vornik.io/vornik/internal/ui/assetschema"
)

// baselineSchemaValues posts the schema-driven form's editable fields
// for the form-demo fixture, keyed on the dotted Path (the HTML name
// the generic renderer emits). Mirrors the fixture so a test changes
// only the field it cares about; without these the optional fields
// would be dropped (RemoveIfEmpty on blank) and the required routing
// fields would fail pre-validation.
func baselineSchemaValues() url.Values {
	v := url.Values{}
	v.Set("displayName", "Form Demo")
	v.Set("swarmId", "swarm-1")
	v.Set("defaultWorkflowId", "workflow-1")
	v.Set("defaultPriority", "50")
	v.Set("maxConcurrentTasks", "1")
	v.Set("autonomy.enabled", "false")
	v.Set("autonomy.mode", "llm")
	v.Set("autonomy.goal", "Original goal line one.\nOriginal goal line two.")
	v.Set("autonomy.maxTasksPerHour", "5")
	v.Set("autonomy.pollInterval", "10m")
	v.Set("autonomy.allowedTaskTypes", "research")
	return v
}

func postSchema(values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/projects/form-demo/config/schema", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Rewriting a project YAML is admin-gated; stamp the admin context
	// the middleware would set in production.
	return withAdminUI(req)
}

// TestProjectSchemaConfigEdit_RendersSchemaForm covers the GET side:
// the generic renderer emits one input per schema field, keyed on the
// dotted path, pre-filled from the project's current YAML, with enums
// as selects and the identity key read-only.
func TestProjectSchemaConfigEdit_RendersSchemaForm(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/schema", nil)
	server.ProjectSchemaConfigEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// Field names are the dotted paths.
	assert.Contains(t, body, `name="displayName"`)
	assert.Contains(t, body, `name="budget.daily_soft_usd"`)
	// Pre-filled current value.
	assert.Contains(t, body, "Form Demo")
	// Enum renders a select with the autonomy mode options.
	assert.Contains(t, body, `name="autonomy.mode"`)
	assert.Contains(t, body, "backlog")
	// Identity key is shown but disabled (read-only).
	assert.Contains(t, body, `name="projectId"`)
	assert.Contains(t, body, "disabled")
}

// TestProjectSchemaConfigSave_RoundTrip is the headline P1c contract:
// render → POST edit → patch → validate → write → reload → re-render
// reflects the change, and unrelated comments / key order survive.
func TestProjectSchemaConfigSave_RoundTrip(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	v := baselineSchemaValues()
	v.Set("displayName", "Renamed Project")
	v.Set("budget.daily_soft_usd", "12.5")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Reloaded exactly once.
	assert.Equal(t, 1, reloader.calls)

	// On-disk file: change applied, comments/scaffold preserved.
	saved, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	got := string(saved)
	assert.Contains(t, got, "Renamed Project")
	assert.Contains(t, got, "daily_soft_usd: 12.5")
	assert.Contains(t, got, "# Banner — preserved verbatim.", "top-of-file comment must survive")
	assert.Contains(t, got, "# budget:", "commented-out scaffold must survive")
	assert.Contains(t, got, "Original goal line one.", "untouched autonomy goal preserved")

	// Registry reflects the change.
	proj := server.projectReg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Equal(t, "Renamed Project", proj.DisplayName)

	// Re-render shows the normalized post-reload value.
	assert.Contains(t, rec.Body.String(), "Renamed Project")
}

// TestProjectSchemaConfigSave_ReadOnlyIdentityNeverPatched proves the
// projectId field can't be rewritten through the form even if a forged
// POST supplies one — editing identity would orphan the config file.
func TestProjectSchemaConfigSave_ReadOnlyIdentityNeverPatched(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	v := baselineSchemaValues()
	v.Set("projectId", "hijacked")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	saved, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	got := string(saved)
	assert.Contains(t, got, "projectId: form-demo")
	assert.NotContains(t, got, "hijacked")
}

// TestProjectSchemaConfigSave_ValidationRejectsBadEnum proves a value
// outside an enum is rejected before any write: 400, per-field error,
// nothing written, no reload.
func TestProjectSchemaConfigSave_ValidationRejectsBadEnum(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	v := baselineSchemaValues()
	v.Set("autonomy.mode", "bogus")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "one of")
	assert.Equal(t, 0, reloader.calls, "must not reload on validation failure")

	saved, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(saved), `displayName: "Form Demo"`, "file untouched on validation failure")
}

// TestProjectSchemaConfigSave_WritesAudit proves a successful save
// records exactly one admin-audit row, closing the audit gap the
// design calls out.
func TestProjectSchemaConfigSave_WritesAudit(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	audit := &stubAdminAuditRepo{}
	server.adminAuditRepo = audit

	v := baselineSchemaValues()
	v.Set("displayName", "Audited")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.Len(t, audit.rows, 1)
	assert.Equal(t, "project.config.save", audit.rows[0].Action)
	assert.Equal(t, "form-demo", audit.rows[0].Target)
}

// TestProjectSchemaConfigEdit_MissingProject covers the not-found
// branch: a project with no YAML on disk renders a 404 with the error.
func TestProjectSchemaConfigEdit_MissingProject(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/ghost/config/schema", nil)
	server.ProjectSchemaConfigEdit(rec, req, "ghost")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Project config not found")
}

// TestProjectSchemaConfigSave_MissingProject covers the save-side
// not-found short-circuit (no write attempted).
func TestProjectSchemaConfigSave_MissingProject(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(baselineSchemaValues()), "ghost")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, 0, reloader.calls)
}

// TestProjectSchemaConfigSave_StructValidationFails covers the post-patch
// struct-validation branch: a syntactically valid value that the registry
// rejects (a workflow id that doesn't resolve) is refused with a 400 and
// nothing is written.
func TestProjectSchemaConfigSave_StructValidationFails(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	v := baselineSchemaValues()
	v.Set("defaultWorkflowId", "does-not-exist")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Validation failed")
	assert.Equal(t, 0, reloader.calls)
}

// TestProjectSchemaConfigSave_ReloadErrorReportsConflict — the write
// succeeds but the daemon can't pick up the change: 409, backup path
// surfaced so the operator can recover.
func TestProjectSchemaConfigSave_ReloadErrorReportsConflict(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&erroringReloader{}))

	v := baselineSchemaValues()
	v.Set("displayName", "Reload Fail")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "reload failed")
	assert.Contains(t, rec.Body.String(), "Backup:")
}

// TestProjectSchemaConfigEdit_DecodeErrorRendersEmpty — a malformed YAML
// on disk can't be decoded for pre-fill, so the form renders with empty
// values rather than erroring (the operator can still re-author it).
func TestProjectSchemaConfigEdit_DecodeErrorRendersEmpty(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "form-demo.yaml"), []byte("\tnot: [valid"), 0o600))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/schema", nil)
	server.ProjectSchemaConfigEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="displayName"`)
}

// TestProjectSchemaConfigEdit_InvalidID covers the id-sanitisation
// branch — a path-bearing id is rejected before any disk access.
func TestProjectSchemaConfigEdit_InvalidID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/x/config/schema", nil)
	server.ProjectSchemaConfigEdit(rec, req, "a/b")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid project id")
}

// TestProjectSchemaConfigSave_PatchErrorOnMalformedYAML covers the
// apply-patch failure branch: an unparseable file on disk can't be
// surgically patched, so the save is refused (400) and nothing written.
func TestProjectSchemaConfigSave_PatchErrorOnMalformedYAML(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	// A scalar top-level document — passes stat, fails the patcher's
	// "top-level must be a mapping" check.
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "form-demo.yaml"), []byte("just-a-string\n"), 0o600))

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(baselineSchemaValues()), "form-demo")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Failed to apply form edits")
	assert.Equal(t, 0, reloader.calls)
}

// TestProjectSchemaConfigSave_RequiresAdmin covers the admin gate: a
// non-admin caller (auth enabled, no admin context) is refused with 403
// before any work.
func TestProjectSchemaConfigSave_RequiresAdmin(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	// No withAdminUI — the default test context has auth enabled but no
	// admin scope, so the mutation gate must reject it.
	req := httptest.NewRequest(http.MethodPost, "/projects/form-demo/config/schema", strings.NewReader(baselineSchemaValues().Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, req, "form-demo")

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, reloader.calls)
}

// TestProjectSchemaConfigEdit_NoConfigDir covers the branch where the
// registry has no config directory wired — the form can't locate any
// asset and reports it rather than panicking.
func TestProjectSchemaConfigEdit_NoConfigDir(t *testing.T) {
	server := NewServer() // no registry → configDir() == ""

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/x/config/schema", nil)
	server.ProjectSchemaConfigEdit(rec, req, "x")

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "config directory is not configured")
}

// TestProjectSchemaConfigSave_ParseFormError covers the malformed-body
// branch — a request whose form body can't be parsed is a 400.
func TestProjectSchemaConfigSave_ParseFormError(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodPost, "/projects/form-demo/config/schema", strings.NewReader("displayName=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, withAdminUI(req), "form-demo")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Failed to parse form")
}

// TestProjectSchemaConfigSave_NoReloaderUsesRegistryLoad covers the
// fallback reload path when no ConfigReloader is wired: the handler
// reloads the registry directly.
func TestProjectSchemaConfigSave_NoReloaderUsesRegistryLoad(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(WithProjectRegistry(reg)) // no reloader

	v := baselineSchemaValues()
	v.Set("displayName", "Registry Reload")

	rec := httptest.NewRecorder()
	server.ProjectSchemaConfigSave(rec, postSchema(v), "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "Registry Reload", reg.GetProject("form-demo").DisplayName)
}

// TestSchemaPatches_SkipsBodyFields proves body-backed fields (prose that
// lives in the markdown body) never become frontmatter YAML patches —
// they route through the body editor instead.
func TestSchemaPatches_SkipsBodyFields(t *testing.T) {
	schema := assetschema.AssetSchema{
		Asset: "demo",
		Sections: []assetschema.Section{{
			Title: "S",
			Fields: []assetschema.Field{
				{Path: "displayName", Kind: assetschema.KindString},
				{Path: "rolePrelude", Kind: assetschema.KindString, Backing: assetschema.BackingBody},
			},
		}},
	}
	values := []assetschema.FieldValue{
		{Path: "displayName", Kind: assetschema.KindString, Value: "x", Provided: true},
		{Path: "rolePrelude", Kind: assetschema.KindString, Value: "prose", Provided: true},
	}
	patches := schemaPatches(schema, values)
	require.Len(t, patches, 1)
	assert.Equal(t, []string{"displayName"}, patches[0].Path)
}

// TestProjectSchemaConfigRoute proves the router dispatches the new
// GET/POST endpoints to the schema handlers.
func TestProjectSchemaConfigRoute(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/schema", nil)
	server.projectRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="displayName"`)
}
