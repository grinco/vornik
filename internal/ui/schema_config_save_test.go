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
)

// Tests for the saveSchemaAsset primitive (the shared schema-driven save
// pipeline behind SwarmSchemaConfigSave / WorkflowSchemaConfigSave). These
// cover the branches the per-asset round-trip suites don't: the parse-form
// early exit (which, post-refactor, routes through the per-asset renderErr
// closure with nil values — the editor must re-render its loaded state, not a
// blank form) and the collection-reconcile rejection path on both the swarm
// sequence and the workflow map.

// badFormRequest builds a POST whose body has invalid percent-encoding so
// r.ParseForm() fails — exercising the skeleton's step-3 early exit.
func badFormRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("%zz=bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return withAdminUI(req)
}

func TestSwarmSchemaConfigSave_ParseFormError(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, badFormRequest("/swarms/edit-swarm/schema"), "edit-swarm")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, reloader.calls, "nothing reloaded on a parse failure")
	body := rec.Body.String()
	assert.Contains(t, body, "Failed to parse form")
	// nil-values render path: the editor re-renders its loaded state (the
	// fixture's role cards) rather than a blanked form.
	assert.Contains(t, body, `name="roles[0]`, "editor must keep its loaded form")
	// File untouched.
	saved, err := os.ReadFile(filepath.Join(root, "swarms", "edit-swarm.md"))
	require.NoError(t, err)
	assert.Contains(t, string(saved), `name: "lead"`)
}

func TestWorkflowSchemaConfigSave_ParseFormError(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, badFormRequest("/workflows/edit-wf/schema"), "edit-wf")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, reloader.calls)
	body := rec.Body.String()
	assert.Contains(t, body, "Failed to parse form")
	assert.Contains(t, body, `name="steps[0]`, "editor must keep its loaded form")
}

// TestSwarmSchemaConfigSave_DuplicateRoleID pins behaviour when two role cards
// share the same name (the sequence IDField). The save must be rejected before
// any write/reload — the reconcile/validate pipeline rejects the collision.
func TestSwarmSchemaConfigSave_DuplicateRoleID(t *testing.T) {
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	v.Set("rolePrelude", "prelude")
	setRole(v, 0, leadRoleFields())
	dup := coderRoleFields()
	dup["name"] = "lead" // collide with card 0
	setRole(v, 1, dup)

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 0, reloader.calls, "no reload on a rejected save")
	// File unchanged — the original two distinct roles survive.
	saved, err := os.ReadFile(filepath.Join(root, "swarms", "edit-swarm.md"))
	require.NoError(t, err)
	assert.Contains(t, string(saved), `name: "coder"`)
}

// TestSwarmSchemaConfigSave_WriteError covers the step-12 write-failure
// branch: a read-only swarms/ dir makes writeProjectConfigAtomic fail, which
// must surface as 500 "Failed to write swarm" with no reload. Skipped under
// root (DAC write checks are bypassed, so the chmod wouldn't deny the write).
func TestSwarmSchemaConfigSave_WriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir does not deny writes when running as root")
	}
	root := writeSwarmFixture(t)
	server, reloader := swarmEditServer(t, root)

	swarmsDir := filepath.Join(root, "swarms")
	require.NoError(t, os.Chmod(swarmsDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(swarmsDir, 0o700) }) // restore so t.TempDir cleanup succeeds

	v := url.Values{}
	v.Set("displayName", "Editable Swarm")
	v.Set("leadRole", "lead")
	v.Set("rolePrelude", "prelude")
	setRole(v, 0, leadRoleFields())

	rec := httptest.NewRecorder()
	server.SwarmSchemaConfigSave(rec, postSwarmSchema("edit-swarm", v), "edit-swarm")

	require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Failed to write swarm")
	assert.Equal(t, 0, reloader.calls)
}

// TestWorkflowSchemaConfigSave_DuplicateStepID pins behaviour when two step
// cards share the same synthetic map key (stepId). The map reconcile path must
// reject before any write/reload.
func TestWorkflowSchemaConfigSave_DuplicateStepID(t *testing.T) {
	root := writeWorkflowFixture(t)
	server, reloader := workflowEditServer(t, root)

	v := url.Values{}
	v.Set("displayName", "Editable Workflow")
	v.Set("version", "1.0.0")
	v.Set("entrypoint", "plan")
	v.Set("maxStepVisits", "3")
	v.Set("maxIterations", "20")
	setStep(v, 0, planStep())
	dup := implementStep()
	dup["stepId"] = "plan" // collide with card 0's key
	setStep(v, 1, dup)

	rec := httptest.NewRecorder()
	server.WorkflowSchemaConfigSave(rec, postWorkflowSchema("edit-wf", v), "edit-wf")

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 0, reloader.calls, "no reload on a rejected save")
}
