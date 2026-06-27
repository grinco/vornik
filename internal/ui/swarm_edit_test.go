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

// writeSwarmFixture seeds a registry root with a project +
// swarm + workflow where the swarm has two roles, each with a
// body-section prompt the editor can target.
func writeSwarmFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "p1.yaml"), []byte(`projectId: p1
displayName: P1
swarmId: edit-swarm
defaultWorkflowId: w1
defaultPriority: 50
maxConcurrentTasks: 1
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "edit-swarm.md"), []byte(`---
# operator comment that must survive form saves
swarmId: edit-swarm
displayName: "Editable Swarm"
leadRole: lead
rolePrelude: |
  You are part of an editable swarm.
roles:
  - name: "lead"
    description: "Plans and delegates"
    model: "test-lead-model"
    modelFallback: "test-lead-fallback"
    runtime:
      image: "vornik-agent:latest"
    permissions:
      allowedTools: ["file_read", "grep"]
  - name: "coder"
    description: "Implements"
    model: "test-coder-model"
    runtime:
      image: "vornik-agent:latest"
---

# Editable Swarm

A swarm used in unit tests for the editor.

## Role prompts

### lead

Plan the work then delegate.

### coder

Implement one subtask at a time.

## Notes

Other body sections must survive role-prompt edits.
`), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "w1.md"), []byte(`---
workflowId: w1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	return root
}

func swarmEditServer(t *testing.T, root string) (*Server, *reloadingReloader) {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))
	return server, reloader
}

func postSwarmForm(swarmID string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/swarms/"+swarmID+"/edit", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// TestSwarmEdit_RendersPerRoleFrontmatterControls — each role
// row exposes inputs for description / model / allowedTools so
// operators can iterate on those fields from the form. Without
// these the form's view of a role was prompt-only and operators
// had to drop to disk to change anything else.
func TestSwarmEdit_RendersPerRoleFrontmatterControls(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`name="roleDescription_lead"`,
		`name="roleModel_lead"`,
		`name="roleModelFallback_lead"`,
		`name="roleAllowedTools_lead"`,
		`name="roleDescription_coder"`,
		`name="roleModel_coder"`,
		`name="roleModelFallback_coder"`,
	} {
		assert.Contains(t, body, want, "swarm editor missing per-role field %q", want)
	}
}

// TestSwarmSave_UpdatesPerRoleFrontmatter — form posts new
// description / model / allowedTools for each role. yaml.Node
// sequence patcher applies them in place; the comment above
// the roles block survives.
func TestSwarmSave_UpdatesPerRoleFrontmatter(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)
	path := filepath.Join(root, "swarms", "edit-swarm.md")

	form := url.Values{}
	form.Set("displayName", "Editable Swarm")
	form.Set("leadRole", "lead")
	form.Set("rolePrelude", "You are part of an editable swarm.")
	form.Set("rolePrompt_lead", "lead body")
	form.Set("rolePrompt_coder", "coder body")
	form.Set("roleDescription_lead", "Plans and ships")         // was: Plans and delegates
	form.Set("roleModel_lead", "test-lead-model-v2")            // was: test-lead-model
	form.Set("roleModelFallback_lead", "test-lead-fallback-v2") // was: test-lead-fallback
	form.Set("roleAllowedTools_lead", "file_read\ngrep\nfile_write")
	form.Set("roleDescription_coder", "Implements features")   // was: Implements
	form.Set("roleModel_coder", "test-coder-model")            // unchanged
	form.Set("roleModelFallback_coder", "test-coder-fallback") // was: (unset)
	form.Set("roleAllowedTools_coder", "")

	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("edit-swarm", form), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	sw := server.projectReg.GetSwarm("edit-swarm")
	require.NotNil(t, sw)
	byName := map[string]registry.SwarmRole{}
	for _, r := range sw.Roles {
		byName[r.Name] = r
	}
	assert.Equal(t, "Plans and ships", byName["lead"].Description)
	assert.Equal(t, "test-lead-model-v2", byName["lead"].Model)
	assert.Equal(t, "test-lead-fallback-v2", byName["lead"].ModelFallback)
	assert.Equal(t, []string{"file_read", "grep", "file_write"}, byName["lead"].Permissions.AllowedTools)
	assert.Equal(t, "Implements features", byName["coder"].Description)
	assert.Equal(t, "test-coder-model", byName["coder"].Model)
	assert.Equal(t, "test-coder-fallback", byName["coder"].ModelFallback)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.Contains(t, got, "# operator comment that must survive form saves")
}

// TestSwarmEdit_RendersModelSelectWhenPricingWired — per-role
// model field becomes a dropdown when the daemon's pricing
// table is wired. Falls back to free-text when no pricing
// (covered separately by TestSwarmEdit_PopulatesFromSwarm).
func TestSwarmEdit_RendersModelSelectWhenPricingWired(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	pricing := pricingFixture(t, "gpt-5.4", "test-lead-model", "claude-4.5-sonnet")
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithAssistantPricing(pricing),
	)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<select name="roleModel_lead"`, "roleModel_lead must render as <select>")
	assert.Contains(t, body, `value="gpt-5.4"`)
	assert.Contains(t, body, `value="claude-4.5-sonnet"`)
}

func TestSwarmEdit_RendersCurrentModelWhenOutsidePricingCatalog(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantPricing(pricingFixture(t, "gpt-5.4")),
	)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<option value="test-lead-model" selected>test-lead-model</option>`)
	assert.Contains(t, body, `<option value="test-coder-model" selected>test-coder-model</option>`)
}

// TestSwarmEdit_RendersAssistAffordance — every role row in
// the swarm editor must carry the AI Assist controls so an
// operator can ask the assistant to draft or critique that
// role's system prompt. Pin both the button labels and the
// data-kind / data-target / data-subject attributes the
// front-end JS reads to assemble the POST.
func TestSwarmEdit_RendersAssistAffordance(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`data-assist-kind="swarm_role"`,
		`data-assist-target="edit-swarm"`,
		`data-assist-subject="lead"`,
		`data-assist-subject="coder"`,
		"Draft",
		"Critique",
	} {
		assert.Contains(t, body, want, "swarm editor missing %q from AI Assist affordance", want)
	}
}

// TestSwarmEdit_PopulatesFromSwarm — the GET handler renders
// the swarm-level scalars + per-role read-only summaries +
// per-role systemPrompt textareas with the current values.
func TestSwarmEdit_PopulatesFromSwarm(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/edit-swarm/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "edit-swarm")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	for _, want := range []string{
		`name="displayName"`,
		`name="leadRole"`,
		`name="rolePrelude"`,
		`name="rolePrompt_lead"`,
		`name="rolePrompt_coder"`,
		"Editable Swarm",
		"You are part of an editable swarm.",
		"Plan the work then delegate.",
		"Implement one subtask at a time.",
		// Read-only per-role summaries surface the model + role
		// description so operators can sanity-check before
		// editing the prompt.
		"test-lead-model",
		"test-lead-fallback",
		"Plans and delegates",
		"file_read",
	} {
		assert.Contains(t, body, want, "rendered swarm editor missing %q", want)
	}
}

// TestSwarmEdit_InvalidID — slash-traversal projectID-style
// inputs are rejected.
func TestSwarmEdit_InvalidID(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/etc/passwd/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "../etc/passwd")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSwarmEdit_UnknownID — a swarm not in the registry returns
// 404 rather than a half-rendered form.
func TestSwarmEdit_UnknownID(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/swarms/no-such/edit", nil)
	rec := httptest.NewRecorder()
	server.SwarmEdit(rec, req, "no-such")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSwarmSave_UpdatesScalarsAndPrompts — happy path: update
// displayName, leadRole, rolePrelude, lead's prompt, coder's
// prompt. Assert (a) the in-memory swarm reflects every change,
// (b) the original operator comment survives the save, (c) the
// `## Notes` section survives.
func TestSwarmSave_UpdatesScalarsAndPrompts(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)
	path := filepath.Join(root, "swarms", "edit-swarm.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := url.Values{}
	form.Set("displayName", "Editable Swarm v2")
	form.Set("leadRole", "coder")
	form.Set("rolePrelude", "Updated prelude.\nSecond line.")
	form.Set("rolePrompt_lead", "Plan only. Delegate everything.")
	form.Set("rolePrompt_coder", "Implement carefully. Test first.")

	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("edit-swarm", form), "edit-swarm")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	sw := server.projectReg.GetSwarm("edit-swarm")
	require.NotNil(t, sw)
	assert.Equal(t, "Editable Swarm v2", sw.DisplayName)
	assert.Equal(t, "coder", sw.LeadRole)
	assert.Contains(t, sw.RolePrelude, "Updated prelude.")
	assert.Contains(t, sw.RolePrelude, "Second line.")
	byName := map[string]registry.SwarmRole{}
	for _, r := range sw.Roles {
		byName[r.Name] = r
	}
	assert.Contains(t, byName["lead"].SystemPrompt, "Plan only. Delegate everything.")
	assert.Contains(t, byName["coder"].SystemPrompt, "Implement carefully. Test first.")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.Contains(t, got, "# operator comment that must survive form saves", "operator comment must be preserved (yaml.Node surgery on frontmatter)")
	assert.Contains(t, got, "## Notes", "non-prompts body section must survive")
	assert.Contains(t, got, "Other body sections must survive role-prompt edits.", "non-prompts body content must survive")

	backups, err := filepath.Glob(path + ".bak-*")
	require.NoError(t, err)
	require.Len(t, backups, 1)
	bak, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(before), string(bak), "backup must hold pre-save content")
}

// TestSwarmSave_ValidationFailure — a save that produces a
// SWARM.md the parser rejects (e.g. leadRole pointing to a
// nonexistent role) returns 400, leaves the file unchanged,
// and does NOT reload.
func TestSwarmSave_ValidationFailure(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)
	path := filepath.Join(root, "swarms", "edit-swarm.md")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := url.Values{}
	form.Set("displayName", "Won't save")
	form.Set("leadRole", "ghost-role") // not in the swarm's roles
	form.Set("rolePrelude", "")
	form.Set("rolePrompt_lead", "x")
	form.Set("rolePrompt_coder", "y")

	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("edit-swarm", form), "edit-swarm")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, reloader.calls)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "file must be untouched on validation failure")
}

// TestSwarmSave_InvalidID — slash-traversal at save time also
// gets the 404 treatment.
func TestSwarmSave_InvalidID(t *testing.T) {
	root := writeSwarmFixture(t)
	server, _ := swarmEditServer(t, root)
	form := url.Values{}
	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("../etc/passwd", form), "../etc/passwd")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTrimSwarmParserPrefix — strip the noisy "SWARM.md <file>:"
// banner so the inline form error shows the actionable reason.
// Pass-through when the prefix doesn't match.
func TestTrimSwarmParserPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SWARM.md test.md: yaml frontmatter parse: bad indent", "yaml frontmatter parse: bad indent"},
		{"plain error without prefix", "plain error without prefix"},
		{"SWARM.md", "SWARM.md"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, trimSwarmParserPrefix(tc.in))
	}
}

// TestSwarmSave_ReloadErrorReportsConflict — file write
// succeeded but the daemon refuses the new config: 409 with
// backup path.
func TestSwarmSave_ReloadErrorReportsConflict(t *testing.T) {
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &erroringReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	form := url.Values{}
	form.Set("displayName", "Editable Swarm")
	form.Set("leadRole", "lead")
	form.Set("rolePrelude", "")
	form.Set("rolePrompt_lead", "x")
	form.Set("rolePrompt_coder", "y")

	rec := httptest.NewRecorder()
	server.SwarmSave(rec, postSwarmForm("edit-swarm", form), "edit-swarm")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 1, reloader.calls)
}
