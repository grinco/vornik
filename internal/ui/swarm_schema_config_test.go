package ui

import (
	"fmt"
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

// setRole sets every roles[i].<field> form value for one card.
func setRole(v url.Values, i int, fields map[string]string) {
	for k, val := range fields {
		v.Set(fmt.Sprintf("roles[%d].%s", i, k), val)
	}
}

// leadRoleFields / coderRoleFields mirror the fixture's current role
// state, so a test that wants a role preserved submits its full field
// set (the form posts the whole collection; a blank field would be
// RemoveIfEmpty'd away).
func leadRoleFields() map[string]string {
	return map[string]string{
		"name":                     "lead",
		"description":              "Plans and delegates",
		"model":                    "test-lead-model",
		"modelFallback":            "test-lead-fallback",
		"runtime.image":            "vornik-agent:latest",
		"permissions.allowedTools": "file_read\ngrep",
		"systemPrompt":             "Plan the work then delegate.",
	}
}

func coderRoleFields() map[string]string {
	return map[string]string{
		"name":          "coder",
		"description":   "Implements",
		"model":         "test-coder-model",
		"runtime.image": "vornik-agent:latest",
		"systemPrompt":  "Implement one subtask at a time.",
	}
}

// newSwarmRegistry loads a registry from root (for tests that build a
// custom server without swarmEditServer's reloader).
func newSwarmRegistry(t *testing.T, root string) *registry.Registry {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func postSwarmSchema(swarmID string, v url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/swarms/"+swarmID+"/schema", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return withAdminUI(req)
}

// TestSwarmSchemaConfigEdit_RendersRoleCards covers the GET side: the
// top-level fields plus one card per role, each card's fields named
// roles[<i>].<path> and pre-filled, with the body systemPrompt as a
// textarea.
func TestSwarmSchemaConfigEdit_RendersRoleCards(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/schema", nil)
	server.SwarmSchemaConfigEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	for _, want := range []string{
		`name="displayName"`,
		`name="leadRole"`,
		`name="rolePrelude"`,
		`name="roles[0].name"`,
		`name="roles[0].model"`,
		`name="roles[0].systemPrompt"`,
		`name="roles[1].name"`,
	} {
		assert.Contains(t, body, want, "missing %q", want)
	}
	// Pre-filled values.
	assert.Contains(t, body, "lead")
	assert.Contains(t, body, "test-lead-model")
	assert.Contains(t, body, "Plan the work then delegate.")
}

// TestSwarmSchemaConfigSave_RoundTrip is the P2b headline: reorder an
// existing role, edit its fields + body prompt, and add a new role —
// then verify the file/registry reflect it and unrelated comments + body
// sections survive.
func TestSwarmSchemaConfigSave_RoundTrip(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	v.Set("rolePrelude", "You are part of an editable swarm.")
	// Card 0 = coder (reordered ahead of lead), model + prompt edited.
	coder := coderRoleFields()
	coder["model"] = "new-coder-model"
	coder["systemPrompt"] = "Implement carefully and test."
	setRole(v, 0, coder)
	// Card 1 = lead (preserved — full field set submitted).
	setRole(v, 1, leadRoleFields())
	// Card 2 = tester (new).
	setRole(v, 2, map[string]string{
		"name":          "tester",
		"description":   "Tests",
		"runtime.image": "vornik-agent:latest",
		"systemPrompt":  "Write tests first.",
	})

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	// Registry reflects reorder + add + edit.
	sw := server.projectReg.GetSwarm("edit-swarm")
	require.NotNil(t, sw)
	names := make([]string, 0, len(sw.Roles))
	for _, r := range sw.Roles {
		names = append(names, r.Name)
	}
	assert.Equal(t, []string{"coder", "lead", "tester"}, names, "reorder + append")
	assert.Equal(t, "new-coder-model", sw.Roles[0].Model)

	// On-disk: operator comment + unrelated body section survive; body
	// prompts updated/added.
	saved, err := os.ReadFile(filepath.Join(root, "swarms", "edit-swarm.md"))
	require.NoError(t, err)
	got := string(saved)
	assert.Contains(t, got, "# operator comment that must survive form saves")
	assert.Contains(t, got, "## Notes")
	assert.Contains(t, got, "Other body sections must survive")
	assert.Contains(t, got, "Implement carefully and test.")
	assert.Contains(t, got, "Write tests first.")
}

// TestSwarmSchemaConfigSave_StripsCRLF is the end-to-end regression guard for
// the config-drift-via-CRLF incident: an HTML <textarea> submits its value
// CRLF-encoded per the HTTP spec, and the save path used to write those bytes
// verbatim into the deployed markdown — re-triggering config-drift-check on
// every UI edit. Feeding CRLF (and a bare CR) through the real save handler
// must persist LF-only bytes on disk.
func TestSwarmSchemaConfigSave_StripsCRLF(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	v := url.Values{}
	// Top-level prose field submitted with CRLF.
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	v.Set("rolePrelude", "First line.\r\nSecond line.")
	// Card 0 = lead, multi-line systemPrompt body with CRLF + a bare CR.
	lead := leadRoleFields()
	lead["systemPrompt"] = "Plan the work.\r\nThen delegate.\rAlways."
	setRole(v, 0, lead)
	// Card 1 = coder (preserved).
	setRole(v, 1, coderRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	saved, err := os.ReadFile(filepath.Join(root, "swarms", "edit-swarm.md"))
	require.NoError(t, err)
	assert.NotContains(t, string(saved), "\r", "persisted file must be LF-only")
	// The content itself must survive, just with LF line endings.
	assert.Contains(t, string(saved), "Plan the work.\nThen delegate.\nAlways.")
}

// TestSwarmSchemaConfigSave_RemovesRole drops an (unreferenced) role by
// omitting its card; reconcile deletes it.
func TestSwarmSchemaConfigSave_RemovesRole(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	v.Set("rolePrelude", "You are part of an editable swarm.")
	setRole(v, 0, leadRoleFields()) // only lead survives; coder dropped

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	sw := server.projectReg.GetSwarm("edit-swarm")
	require.NotNil(t, sw)
	require.Len(t, sw.Roles, 1)
	assert.Equal(t, "lead", sw.Roles[0].Name)
}

// TestSwarmSchemaConfigSave_DanglingLeadRoleRejected — referential
// safety: a leadRole that names no submitted role is rejected by struct
// Validate before any write.
func TestSwarmSchemaConfigSave_DanglingLeadRoleRejected(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "ghost") // no such role
	setRole(v, 0, leadRoleFields())
	setRole(v, 1, coderRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, reloader.calls)
	// File unchanged.
	saved, err := os.ReadFile(filepath.Join(root, "swarms", "edit-swarm.md"))
	require.NoError(t, err)
	assert.Contains(t, string(saved), "name: \"lead\"")
}

// TestSwarmSchemaConfigSave_RequiredRoleFieldRejected — a card missing
// the required name is rejected with a per-field error, nothing written.
func TestSwarmSchemaConfigSave_RequiredRoleFieldRejected(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	setRole(v, 0, leadRoleFields())
	// Card 1: blank name (required) but a model set.
	setRole(v, 1, map[string]string{"model": "x", "runtime.image": "img"})

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "required")
	assert.Equal(t, 0, reloader.calls)
}

// TestSwarmSchemaBlankRoleCard serves a blank role card partial for the
// htmx "Add role" control, at the given index.
func TestSwarmSchemaBlankRoleCard(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/schema/role?index=4", nil)
	server.SwarmSchemaRoleCard(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `name="roles[4].name"`)
	assert.Contains(t, body, `name="roles[4].systemPrompt"`)
}

// TestSwarmSchemaConfigEdit_MissingSwarm covers the not-found branch.
func TestSwarmSchemaConfigEdit_MissingSwarm(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/ghost/schema", nil)
	server.SwarmSchemaConfigEdit(rec, req, "ghost")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Swarm not found")
}

// TestSwarmSchemaConfigSave_RequiresAdmin — the save is admin-gated.
func TestSwarmSchemaConfigSave_RequiresAdmin(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	v := url.Values{}
	setRole(v, 0, leadRoleFields())
	req := httptest.NewRequest(http.MethodPost, "/swarms/edit-swarm/schema", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, req, "edit-swarm") // no withAdminUI

	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, 0, reloader.calls)
}

// TestSwarmSchemaConfigSave_MissingSwarm covers the save-side 404.
func TestSwarmSchemaConfigSave_MissingSwarm(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("ghost", url.Values{}), "ghost")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSwarmSchemaConfigSave_WritesAudit — a successful save records one
// admin-audit row.
func TestSwarmSchemaConfigSave_WritesAudit(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	audit := &stubAdminAuditRepo{}
	server.adminAuditRepo = audit

	v := url.Values{}
	v.Set("leadRole", "lead")
	setRole(v, 0, leadRoleFields())
	setRole(v, 1, coderRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.Len(t, audit.rows, 1)
	assert.Equal(t, "swarm.save", audit.rows[0].Action)
	assert.Equal(t, "edit-swarm", audit.rows[0].Target)
}

// TestSwarmSchemaConfigSave_ReloadConflict — write succeeds, reload
// fails → 409 with backup path.
func TestSwarmSchemaConfigSave_ReloadConflict(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := newSwarmRegistry(t, root)
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(&erroringReloader{}))

	v := url.Values{}
	v.Set("leadRole", "lead")
	setRole(v, 0, leadRoleFields())
	setRole(v, 1, coderRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "reload failed")
}

// TestSwarmSchemaConfigSave_NoReloaderUsesRegistryLoad covers the
// fallback reload path (no ConfigReloader wired).
func TestSwarmSchemaConfigSave_NoReloaderUsesRegistryLoad(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := newSwarmRegistry(t, root)
	server := NewServer(WithProjectRegistry(reg)) // no reloader

	v := url.Values{}
	v.Set("displayName", "Renamed Swarm")
	v.Set("leadRole", "lead")
	setRole(v, 0, leadRoleFields())
	setRole(v, 1, coderRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "Renamed Swarm", reg.GetSwarm("edit-swarm").DisplayName)
}

// TestSwarmSchemaRoute proves the router dispatches the schema endpoints.
func TestSwarmSchemaRoute(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/schema", nil)
	server.swarmRouter(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `name="roles[0].name"`)
}
