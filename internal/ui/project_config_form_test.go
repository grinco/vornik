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
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// reloadingReloader is a test ConfigReloader that actually
// reloads the registry from disk — unlike mockConfigReloader
// which just bumps a counter. Needed because the form handler
// re-reads the project from the registry after save so the
// rendered form reflects post-validation state; assertions on
// that state require the registry to actually re-load.
type reloadingReloader struct {
	reg   *registry.Registry
	root  string
	calls int
}

func (r *reloadingReloader) Reload() error {
	r.calls++
	return r.reg.Load(r.root)
}

// commentedProjectYAML is the fixture used by every save test.
// It mirrors the structure of bundled assistant-project.yaml:
// inline header comment, commented sibling, a populated autonomy
// block with prose comments, an empty permissions block, and a
// commented-out budget scaffold. The point of every test below
// is that the patcher preserves these.
const commentedProjectYAML = `# Banner — preserved verbatim.
projectId: form-demo
displayName: "Form Demo"
swarmId: swarm-1
defaultWorkflowId: workflow-1
defaultPriority: 50
maxConcurrentTasks: 1

# autonomy block — prose comment preserved.
autonomy:
  enabled: false
  # goal is the high-level objective.
  goal: |
    Original goal line one.
    Original goal line two.
  mode: llm
  maxTasksPerHour: 5
  pollInterval: "10m"
  allowedTaskTypes:
    - "research"

permissions:
  secrets: []
  allowedTools:
    - "current_time"
    - "file_read"

# budget:
#   daily_hard_usd: 20.0
`

// writeFormFixture seeds a config directory with a project +
// swarm + workflow trio sufficient for the form save path to
// validate cleanly. Returns the root path.
func writeFormFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "form-demo.yaml"), []byte(commentedProjectYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-1.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: coder
    model: test-model
    systemPrompt: code
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "workflow-1.md"), []byte(`---
workflowId: workflow-1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: coder
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	return root
}

// formServer returns a Server wired with a registry loaded from
// root + a reloader that re-reads the registry on each save.
// Used by tests that need the post-save form refresh to reflect
// disk state.
func formServer(t *testing.T, root string) (*Server, *reloadingReloader) {
	t.Helper()
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))
	return server, reloader
}

// postForm builds a POST request mimicking the form editor's
// submission. Callers pass the form values they want set; this
// keeps each test focused on the values that matter, not the
// boilerplate of every form field.
func postForm(values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/projects/form-demo/config/form", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// pricingFixture builds a *pricing.Table seeded with the
// given model ids so the form's model dropdown has known
// options to render. Each model gets a token-cost entry so
// downstream cost calc still works.
func pricingFixture(t *testing.T, modelIDs ...string) *pricing.Table {
	t.Helper()
	var b strings.Builder
	b.WriteString("models:\n")
	for _, id := range modelIDs {
		b.WriteString("  \"" + id + "\":\n    input: 1.0\n    output: 2.0\n")
	}
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)
	return table
}

// baselineFormValues returns the minimum set of form fields a
// real browser would post for the form-demo fixture. Each test
// overrides only what it cares about — without these defaults
// the form save zeros out required routing fields (swarmId,
// defaultWorkflowId) because RemoveIfEmpty fires on blank
// inputs.
func baselineFormValues() url.Values {
	v := url.Values{}
	v.Set("displayName", "Form Demo")
	v.Set("swarmId", "swarm-1")
	v.Set("defaultWorkflowId", "workflow-1")
	v.Set("defaultPriority", "50")
	v.Set("maxConcurrentTasks", "1")
	v.Set("autonomy_enabled", "false")
	v.Set("autonomy_mode", "llm")
	v.Set("autonomy_maxTasksPerHour", "5")
	v.Set("autonomy_pollInterval", "10m")
	v.Set("autonomy_allowedTaskTypes", "research")
	return v
}

// TestSplitChipList covers the textarea→[]string conversion
// helper: trim, dedupe, mixed newline + comma separators, empty
// lines, and leading/trailing whitespace.
func TestSplitChipList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"single", "foo", []string{"foo"}},
		{"newlines", "a\nb\nc", []string{"a", "b", "c"}},
		{"commas", "a, b, c", []string{"a", "b", "c"}},
		{"mixed", "a, b\nc", []string{"a", "b", "c"}},
		{"trim whitespace", "  a  \n  b  ", []string{"a", "b"}},
		{"dedupe", "a\nb\na\nc\nb", []string{"a", "b", "c"}},
		{"skip blanks", "a\n\n\nb", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitChipList(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseFormBool covers every accepted truthy + falsy form
// the helper supports. The `<select value="true">` shape and the
// `<input type="checkbox" />` "on" / missing shape both work.
func TestParseFormBool(t *testing.T) {
	for _, in := range []string{"true", "TRUE", "on", "1", "yes", "Yes"} {
		assert.Truef(t, parseFormBool(in), "%q should be true", in)
	}
	for _, in := range []string{"", "false", "0", "no", "off", "random"} {
		assert.Falsef(t, parseFormBool(in), "%q should be false", in)
	}
}

// TestParseFormInt — blank / non-numeric default to 0; numeric
// passes through; negative is preserved so the project validator
// gets to surface a meaningful error downstream.
func TestParseFormInt(t *testing.T) {
	assert.Equal(t, 0, parseFormInt(""))
	assert.Equal(t, 0, parseFormInt("   "))
	assert.Equal(t, 0, parseFormInt("not-a-number"))
	assert.Equal(t, 25, parseFormInt("25"))
	assert.Equal(t, 25, parseFormInt("  25  "))
	assert.Equal(t, -3, parseFormInt("-3"))
}

// TestFirstNonEmptyLine — skips leading blanks, returns the
// first non-blank line, truncates anything over 120 chars with
// an ellipsis. Used for the brief-card teaser; truncation keeps
// the card a single visual line.
func TestFirstNonEmptyLine(t *testing.T) {
	assert.Equal(t, "", firstNonEmptyLine(""))
	assert.Equal(t, "", firstNonEmptyLine("\n\n   \n"))
	assert.Equal(t, "hello", firstNonEmptyLine("\n\nhello\nworld"))
	assert.Equal(t, "first", firstNonEmptyLine("first\nsecond"))
	long := strings.Repeat("x", 200)
	got := firstNonEmptyLine(long)
	// 120 ASCII bytes from t[:120] + 3-byte UTF-8 ellipsis "…".
	assert.Equal(t, 123, len(got))
	assert.True(t, strings.HasSuffix(got, "…"))
}

// TestBuildFormPatches_PathOrderAndShape — the patcher relies on
// patch order to synthesise intermediate maps; the build helper
// must emit nested paths after their parents so the patch list
// stays well-formed. Also confirms RemoveIfEmpty is set on
// optional fields (so blank input deletes the key rather than
// writing key: "").
func TestBuildFormPatches_PathOrderAndShape(t *testing.T) {
	data := &ProjectConfigFormData{
		DisplayName:     "X",
		AutonomyEnabled: true,
	}
	patches := buildFormPatches(data)
	// 45 patches across Identity / Routing / Autonomy /
	// Permissions / Budget / Rate limit / Retention / Chat /
	// Judge / Trading / GitHub App. Counted explicitly so
	// adding/removing a form field surfaces here rather than
	// silently shifting structure.
	require.Len(t, patches, 45)

	// Per-block presence: each top-level prefix appears at
	// least once, in the expected emit order. Routing fields
	// are top-level (no parent), so we test them by name in the
	// boolean check below rather than as a prefix here.
	wantOrder := []string{"autonomy", "permissions", "budget", "rate_limit", "retention", "chat", "hallucinationJudge", "trading", "github_app"}
	last := -1
	for _, prefix := range wantOrder {
		idx := -1
		for i, p := range patches {
			if len(p.Path) >= 1 && p.Path[0] == prefix {
				idx = i
				break
			}
		}
		require.Greater(t, idx, last, "block %q should come after the previous block", prefix)
		last = idx
	}

	// Boolean fields must NOT have RemoveIfEmpty — the operator's
	// explicit "off" must survive a save. Strings / numbers /
	// sequences DO have RemoveIfEmpty so empty inputs delete keys.
	boolPaths := map[string]bool{
		"autonomy.enabled":           true,
		"autonomy.requireApproval":   true,
		"hallucinationJudge.enabled": true,
	}
	for _, p := range patches {
		key := strings.Join(p.Path, ".")
		if boolPaths[key] {
			if key == "autonomy.requireApproval" {
				// Has RemoveIfEmpty: a requireApproval=false rarely
				// needs to litter every project YAML. Acceptable.
				continue
			}
			assert.Falsef(t, p.RemoveIfEmpty, "bool field %s should not be RemoveIfEmpty", key)
		}
	}
	// displayName always has RemoveIfEmpty (Identity tab covers
	// the inherit-from-projectId fallback).
	assert.True(t, patches[0].RemoveIfEmpty)
}

// TestProjectConfigFormEdit_PopulatesFromProject — the GET
// handler renders a form whose initial values reflect the
// in-registry project state. Verified by checking the rendered
// HTML contains the project's current displayName + autonomy
// goal.
func TestProjectConfigFormEdit_PopulatesFromProject(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Form Demo", "displayName should appear in rendered form")
	assert.Contains(t, body, "Original goal line one.", "autonomy.goal should appear in rendered form")
	assert.Contains(t, body, "current_time", "permissions.allowedTools should appear in rendered form")
}

// TestProjectConfigFormEdit_InvalidProjectID — slashes in the
// project id are rejected (path-traversal defence inherited from
// the YAML editor). Should return 404 not 500.
func TestProjectConfigFormEdit_InvalidProjectID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/etc/passwd/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "../etc/passwd")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid project id")
}

// TestProjectConfigFormEdit_MissingProjectFile — GET against a
// project ID with no YAML on disk returns 404 rather than
// rendering an empty form. Avoids surprising "save" on a brand-
// new file the operator didn't realise wasn't there.
func TestProjectConfigFormEdit_MissingProjectFile(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/no-such/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "no-such")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not found")
}

// TestProjectConfigFormSave_HappyPath — update displayName +
// autonomy goal via the form. Asserts:
//   - file is written
//   - reloader fires (registry reloads)
//   - banner comment + commented-out budget scaffold survive
//   - the post-save form reflects the new values
//   - a timestamped backup of the prior content was created
func TestProjectConfigFormSave_HappyPath(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineFormValues()
	form.Set("displayName", "Renamed Demo")
	form.Set("description", "")
	form.Set("autonomy_goal", "Updated goal line one.\nUpdated goal line two.")
	form.Set("autonomy_maxTasksPerHour", "10")
	form.Set("autonomy_pollInterval", "15m")
	form.Set("autonomy_requireApproval", "false")
	form.Set("autonomy_allowedTaskTypes", "research\nwriting")
	form.Set("permissions_secrets", "")
	// Non-builtin tools go in the custom textarea; the builtin
	// checkbox grid stays empty in this fixture.
	form.Set("permissions_allowedTools_custom", "current_time\nfile_read\ngrep")

	req := postForm(form)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)
	assert.Contains(t, rec.Body.String(), "Project config saved and reloaded")
	assert.Contains(t, rec.Body.String(), "Renamed Demo", "rendered form should reflect post-save state")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.Contains(t, got, `displayName: "Renamed Demo"`)
	assert.Contains(t, got, "Updated goal line one.")
	assert.Contains(t, got, "Updated goal line two.")
	assert.Contains(t, got, `- "grep"`)
	assert.Contains(t, got, "# Banner — preserved verbatim.")
	assert.Contains(t, got, "# autonomy block — prose comment preserved.")
	assert.Contains(t, got, "# budget:")

	backups, err := filepath.Glob(filepath.Join(root, "projects", "form-demo.yaml.bak-*"))
	require.NoError(t, err)
	require.Len(t, backups, 1)
	backup, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(t, string(before), string(backup), "backup should hold pre-save content")
}

// TestProjectConfigFormSave_RemovesEmptyOptional — clearing
// autonomy.goal via the form deletes the key from YAML rather
// than writing `goal: ""`. Confirms the RemoveIfEmpty behaviour
// reaches the disk via the handler path.
func TestProjectConfigFormSave_RemovesEmptyOptional(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")

	form := baselineFormValues()
	form.Set("autonomy_goal", "") // clear
	form.Set("permissions_allowedTools", "current_time")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(after)
	assert.NotContains(t, got, "goal:", "cleared autonomy.goal should be absent from the YAML")
}

// TestProjectConfigFormSave_ValidationFailureKeepsFileIntact —
// when the post-patch YAML fails the sandbox registry load
// (here: swarmId set to something that doesn't resolve), the
// handler returns 400, leaves the original file untouched, and
// does NOT fire the reloader. Mirrors the existing YAML-editor
// validation contract.
func TestProjectConfigFormSave_ValidationFailureKeepsFileIntact(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// Pre-corrupt the YAML so applyYAMLPatches succeeds but the
	// sandbox-load fails — by overwriting swarmId to a missing
	// swarm via a manual edit to the source file BEFORE the form
	// save. (Cleanest way to trigger validation failure since the
	// form fields we expose all map to valid values.)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\n# tail comment\n")...), 0o600))
	before2, _ := os.ReadFile(path)
	// Now wipe swarmId via a separate write so the field is
	// missing — the validator will fail with "swarm not found"
	// or similar.
	bad := strings.Replace(string(before2), "swarmId: swarm-1", "swarmId: nonexistent-swarm", 1)
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))
	prev, _ := os.ReadFile(path)

	form := url.Values{}
	form.Set("displayName", "Triggered Save")
	form.Set("autonomy_enabled", "false")
	form.Set("autonomy_mode", "llm")
	form.Set("autonomy_maxTasksPerHour", "5")
	form.Set("autonomy_pollInterval", "10m")
	form.Set("autonomy_allowedTaskTypes", "research")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Validation failed")
	assert.Equal(t, 0, reloader.calls)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(prev), string(after), "file should be untouched on validation failure")
}

// TestProjectConfigFormSave_InvalidNumberKeepsFileIntact —
// malformed numeric POST values must not collapse to zero and
// delete existing optional keys.
func TestProjectConfigFormSave_InvalidNumberKeepsFileIntact(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineFormValues()
	form.Set("defaultPriority", "urgent")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "defaultPriority must be an integer")
	assert.Equal(t, 0, reloader.calls)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestProjectConfigFormSave_InvalidInt64KeepsFileIntact(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	form := baselineFormValues()
	form.Set("githubApp_appID", "not-a-number")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "githubApp_appID must be a 64-bit integer")
	assert.Equal(t, 0, reloader.calls)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// TestProjectConfigFormSave_InvalidProjectID — POST with a
// path-traversal projectId returns 404, file not written.
func TestProjectConfigFormSave_InvalidProjectID(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	form := url.Values{}
	form.Set("displayName", "x")
	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "../etc/passwd")

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestProjectConfigFormEdit_PopulatesRoutingFields — the
// Routing section's five form fields must surface with their
// current values + the dropdown option lists for swarmId and
// defaultWorkflowId. Without dropdowns operators have to type
// IDs from memory; the LLD's "Routing" bullet explicitly
// requires "Dropdowns populated from the swarm / workflow
// registry, not free-text".
func TestProjectConfigFormEdit_PopulatesRoutingFields(t *testing.T) {
	root := writeFormFixture(t)
	// Add a second swarm + workflow so the dropdown has more
	// than the one the project currently references.
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-2.md"), []byte(`---
swarmId: swarm-2
roles:
  - name: writer
    model: test-model
    systemPrompt: write
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "workflow-2.md"), []byte(`---
workflowId: workflow-2
entrypoint: write
steps:
  write:
    type: agent
    prompt: "write"
    role: writer
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	// Seed defaultPriority / maxConcurrentTasks /
	// adaptiveCandidateWorkflows so populate has something to
	// surface — the base fixture only sets the simplest fields.
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
adaptiveCandidateWorkflows:
  - "workflow-1"
  - "workflow-2"
`)...), 0o600))
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`name="swarmId"`,
		`name="defaultWorkflowId"`,
		`name="adaptiveCandidateWorkflows"`,
		`name="defaultPriority"`,
		`name="maxConcurrentTasks"`,
	} {
		assert.Contains(t, body, want, "v4 Routing form field %s must be rendered", want)
	}
	// Dropdown options: every registered swarm/workflow id
	// appears as an <option> value.
	for _, want := range []string{
		`value="swarm-1"`, `value="swarm-2"`,
		`value="workflow-1"`, `value="workflow-2"`,
	} {
		assert.Contains(t, body, want, "dropdown option %s must be rendered", want)
	}
	// adaptiveCandidateWorkflows surfaces as a textarea body
	// with one workflow per line.
	assert.Contains(t, body, "workflow-1\nworkflow-2", "adaptiveCandidateWorkflows textarea body must preserve order")
}

// TestProjectConfigFormSave_RoutingRoundTrip — saving the
// Routing section writes the values back to disk and a fresh
// load surfaces them. Includes the chip-list path for
// adaptiveCandidateWorkflows.
func TestProjectConfigFormSave_RoutingRoundTrip(t *testing.T) {
	root := writeFormFixture(t)
	// Provision the alternate swarm/workflow we'll switch to.
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-2.md"), []byte(`---
swarmId: swarm-2
roles:
  - name: writer
    model: test-model
    systemPrompt: write
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "workflow-2.md"), []byte(`---
workflowId: workflow-2
entrypoint: write
steps:
  write:
    type: agent
    prompt: "write"
    role: writer
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	server, reloader := formServer(t, root)

	form := url.Values{}
	// Identity + autonomy + permissions need to be sent for the
	// save to clear validation.
	form.Set("displayName", "Form Demo")
	form.Set("autonomy_enabled", "false")
	form.Set("autonomy_mode", "llm")
	form.Set("autonomy_maxTasksPerHour", "5")
	form.Set("autonomy_pollInterval", "10m")
	form.Set("autonomy_allowedTaskTypes", "research")
	// Routing — switch swarm + workflow, add a second adaptive
	// candidate, bump the concurrency caps.
	form.Set("swarmId", "swarm-2")
	form.Set("defaultWorkflowId", "workflow-2")
	form.Set("adaptiveCandidateWorkflows", "workflow-1\nworkflow-2")
	form.Set("defaultPriority", "25")
	form.Set("maxConcurrentTasks", "4")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	proj := server.projectReg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Equal(t, "swarm-2", proj.SwarmID)
	assert.Equal(t, "workflow-2", proj.DefaultWorkflowID)
	assert.Equal(t, []string{"workflow-1", "workflow-2"}, proj.AdaptiveCandidateWorkflows)
	assert.Equal(t, 25, proj.DefaultPriority)
	assert.Equal(t, 4, proj.MaxConcurrentTasks)
}

// TestProjectConfigFormEdit_PopulatesBudgetRateRetentionChatJudge
// — the GET handler must surface every field added in form v3
// so the operator sees their existing config before editing.
// Without this assertion the new template sections could render
// blank inputs even when the YAML has values — a silent data
// hazard if the operator saves without realising the form
// dropped them.
func TestProjectConfigFormEdit_PopulatesBudgetRateRetentionChatJudge(t *testing.T) {
	root := writeFormFixture(t)
	// Append the v3 sections to the project YAML so the loader
	// surfaces them; the base fixture only carries Identity /
	// Autonomy / Permissions.
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	add := []byte(`
budget:
  daily_soft_usd: 5.0
  daily_hard_usd: 20.0
  monthly_soft_usd: 50.0
  monthly_hard_usd: 200.0
  timezone: "Europe/Prague"
rate_limit:
  tasks_per_minute: 4
  tasks_per_hour: 60
retention:
  task_llm_usage_days: 90
  tool_audit_days: 30
  tasks_days: 60
  executions_days: 60
  artifacts_days: 60
chat:
  system_prefix: "house rule paragraph"
hallucinationJudge:
  enabled: true
  model: "gpt-4o-mini"
  prompt: "Score the answer."
`)
	require.NoError(t, os.WriteFile(path, append(existing, add...), 0o600))
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		`name="budget_dailySoftUsd"`,
		`name="budget_dailyHardUsd"`,
		`name="budget_monthlySoftUsd"`,
		`name="budget_monthlyHardUsd"`,
		`name="budget_timezone"`,
		`name="rateLimit_tasksPerMinute"`,
		`name="rateLimit_tasksPerHour"`,
		`name="retention_taskLLMUsageDays"`,
		`name="retention_toolAuditDays"`,
		`name="retention_tasksDays"`,
		`name="retention_executionsDays"`,
		`name="retention_artifactsDays"`,
		`name="chat_systemPrefix"`,
		`name="judge_enabled"`,
		`name="judge_model"`,
		`name="judge_prompt"`,
	} {
		assert.Contains(t, body, want, "v3 form field %s must be rendered", want)
	}
	// Spot-check values surface (input value="…" or textarea body).
	assert.Contains(t, body, "Europe/Prague")
	assert.Contains(t, body, "house rule paragraph")
	assert.Contains(t, body, "gpt-4o-mini")
	assert.Contains(t, body, "Score the answer.")
}

// TestProjectConfigFormSave_BudgetRoundTrip — saving via the
// form writes Budget/Rate/Retention/Chat/Judge fields to YAML
// and the values round-trip back through the registry. Pins
// determinism for the most-touched non-prompt fields.
func TestProjectConfigFormSave_BudgetRoundTrip(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	form := baselineFormValues()
	// v3 fields under test:
	form.Set("budget_dailySoftUsd", "5.00")
	form.Set("budget_dailyHardUsd", "20.50")
	form.Set("budget_monthlySoftUsd", "50")
	form.Set("budget_monthlyHardUsd", "200")
	form.Set("budget_timezone", "Europe/Prague")
	form.Set("rateLimit_tasksPerMinute", "4")
	form.Set("rateLimit_tasksPerHour", "60")
	form.Set("retention_taskLLMUsageDays", "90")
	form.Set("retention_toolAuditDays", "30")
	form.Set("retention_tasksDays", "60")
	form.Set("retention_executionsDays", "60")
	form.Set("retention_artifactsDays", "60")
	form.Set("chat_systemPrefix", "HOUSE RULE: stay concise")
	form.Set("judge_enabled", "true")
	form.Set("judge_model", "gpt-4o-mini")
	form.Set("judge_prompt", "Score the answer.\nUse 0-10.")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	proj := server.projectReg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.InDelta(t, 5.0, proj.Budget.DailySoftUSD, 1e-9)
	assert.InDelta(t, 20.5, proj.Budget.DailyHardUSD, 1e-9)
	assert.InDelta(t, 50.0, proj.Budget.MonthlySoftUSD, 1e-9)
	assert.InDelta(t, 200.0, proj.Budget.MonthlyHardUSD, 1e-9)
	assert.Equal(t, "Europe/Prague", proj.Budget.Timezone)
	assert.Equal(t, 4, proj.RateLimit.TasksPerMinute)
	assert.Equal(t, 60, proj.RateLimit.TasksPerHour)
	assert.Equal(t, 90, proj.Retention.TaskLLMUsageDays)
	assert.Equal(t, 30, proj.Retention.ToolAuditDays)
	assert.Equal(t, 60, proj.Retention.TasksDays)
	assert.Equal(t, 60, proj.Retention.ExecutionsDays)
	assert.Equal(t, 60, proj.Retention.ArtifactsDays)
	assert.Equal(t, "HOUSE RULE: stay concise", proj.Chat.SystemPrefix)
	assert.True(t, proj.HallucinationJudge.Enabled)
	assert.Equal(t, "gpt-4o-mini", proj.HallucinationJudge.Model)
	assert.Contains(t, proj.HallucinationJudge.Prompt, "Score the answer.")
	assert.Contains(t, proj.HallucinationJudge.Prompt, "Use 0-10.")
}

// TestProjectConfigFormSave_BudgetClearsKeysOnEmpty — clearing
// every budget field via the form removes the corresponding
// keys from disk (rather than writing 0.0 everywhere). Lets the
// loader's "zero disables" semantics fire as intended. Uses an
// isolated fixture without the commented-out budget scaffold so
// assertions match real keys, not comment text.
func TestProjectConfigFormSave_BudgetClearsKeysOnEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	projectPath := filepath.Join(root, "projects", "clear-demo.yaml")
	require.NoError(t, os.WriteFile(projectPath, []byte(`projectId: clear-demo
displayName: "Clear Demo"
swarmId: swarm-1
defaultWorkflowId: workflow-1
defaultPriority: 50
maxConcurrentTasks: 1
permissions:
  allowedTools:
    - "current_time"
budget:
  daily_hard_usd: 20.0
  monthly_hard_usd: 200.0
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm-1.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: coder
    model: test-model
    systemPrompt: code
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "workflow-1.md"), []byte(`---
workflowId: workflow-1
entrypoint: build
steps:
  build:
    type: agent
    prompt: "do work"
    role: coder
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	server, _ := formServer(t, root)

	form := url.Values{}
	// All form fields the browser would send — empty inputs for
	// every budget field, real values for everything else.
	form.Set("displayName", "Clear Demo")
	form.Set("swarmId", "swarm-1")
	form.Set("defaultWorkflowId", "workflow-1")
	form.Set("defaultPriority", "50")
	form.Set("maxConcurrentTasks", "1")
	form.Set("autonomy_enabled", "false")
	form.Set("autonomy_mode", "llm")
	form.Set("autonomy_maxTasksPerHour", "5")
	form.Set("autonomy_pollInterval", "10m")
	form.Set("autonomy_allowedTaskTypes", "research")
	form.Set("permissions_allowedTools", "current_time")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/clear-demo/config/form", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.ProjectConfigFormSave(rec, req, "clear-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	after, err := os.ReadFile(projectPath)
	require.NoError(t, err)
	got := string(after)
	assert.NotContains(t, got, "daily_hard_usd:")
	assert.NotContains(t, got, "monthly_hard_usd:")
}

// TestParseFormFloat — float parser used by the budget fields.
// Blanks and unparseable input default to 0; valid inputs pass
// through. Negative values preserved so the project validator
// surfaces them downstream.
func TestParseFormFloat(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"   ", 0},
		{"junk", 0},
		{"5", 5},
		{"5.50", 5.5},
		{"-2.5", -2.5},
		{"  10.10  ", 10.10},
	}
	for _, tc := range cases {
		got := parseFormFloat(tc.in)
		assert.InDeltaf(t, tc.want, got, 1e-9, "parseFormFloat(%q)", tc.in)
	}
}

// erroringReloader fails on every Reload — covers the save
// path that writes successfully but reports a conflict because
// the daemon couldn't pick up the change.
type erroringReloader struct{ calls int }

func (e *erroringReloader) Reload() error {
	e.calls++
	return assert.AnError
}

// TestProjectConfigFormSave_ReloadErrorReportsConflict — when
// the file write succeeds but the daemon refuses the new
// config, the operator must see that distinction (409 not 200)
// and the backup path so they can recover. Mirrors the YAML
// editor's behaviour.
func TestProjectConfigFormSave_ReloadErrorReportsConflict(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &erroringReloader{}
	server := NewServer(WithProjectRegistry(reg), WithConfigReloader(reloader))

	form := baselineFormValues()
	form.Set("displayName", "Reload Fail Demo")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, 1, reloader.calls)
	assert.Contains(t, rec.Body.String(), "reload failed")
}

// TestProjectConfigFormSave_ProjectRegFallback — when no
// ConfigReloader is wired, the handler falls back to reloading
// the project registry directly. That path covers the
// "embedded daemon without a separate reloader" deployment.
func TestProjectConfigFormSave_ProjectRegFallback(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	// No WithConfigReloader — the fallback path fires instead.
	server := NewServer(WithProjectRegistry(reg))

	form := baselineFormValues()
	form.Set("displayName", "Fallback Demo")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	// The fallback reloads the registry directly; the renamed
	// project should be visible via GetProject.
	got := reg.GetProject("form-demo")
	require.NotNil(t, got)
	assert.Equal(t, "Fallback Demo", got.DisplayName)
}

// TestProjectConfigFormSave_PatchErrorReturns400 — pre-corrupt
// the YAML so that one of the form's patch paths walks into a
// non-mapping. The patcher refuses (rather than silently
// overwriting), the handler returns 400, and the file stays as
// it was.
func TestProjectConfigFormSave_PatchErrorReturns400(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	// Replace `autonomy:` mapping with a scalar so any nested
	// autonomy.* patch hits the non-mapping refusal.
	require.NoError(t, os.WriteFile(path, []byte(`projectId: form-demo
swarmId: swarm-1
defaultWorkflowId: workflow-1
autonomy: "this is a string, not a map"
`), 0o600))
	before, _ := os.ReadFile(path)

	form := url.Values{}
	form.Set("displayName", "Anything")
	form.Set("autonomy_enabled", "true")
	form.Set("autonomy_mode", "llm")
	form.Set("autonomy_maxTasksPerHour", "5")
	form.Set("autonomy_pollInterval", "10m")
	form.Set("autonomy_allowedTaskTypes", "research")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "apply form edits")
	assert.Equal(t, 0, reloader.calls)
	after, _ := os.ReadFile(path)
	assert.Equal(t, string(before), string(after))
}

// TestProjectConfigFormEdit_LinksToWorkflowEditor — symmetric
// to the swarm-link case: the defaultWorkflowId dropdown gets
// an "Edit prompts" link to the workflow editor. The projectId
// is carried through as a query param so the assistant grounds
// on the right project's brief.
func TestProjectConfigFormEdit_LinksToWorkflowEditor(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `href="/ui/workflows/workflow-1/edit?projectId=form-demo"`)
}

// TestProjectConfigFormEdit_LinksToSwarmEditor — the Routing
// section must surface a link to the swarm editor with the
// projectId carried through as a query param so the prompt
// assistant grounds on the right project's brief.
func TestProjectConfigFormEdit_LinksToSwarmEditor(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `href="/ui/swarms/swarm-1/edit?projectId=form-demo"`, "swarm editor link must carry projectId")
}

// TestProjectConfigFormEdit_RendersTradingAndGitHubAppFields —
// the project config form must surface Trading + GitHubApp form
// fields so operators no longer drop to Advanced YAML for those
// sections. Pin the field names rather than full HTML so a
// later layout refactor doesn't invalidate the test.
func TestProjectConfigFormEdit_RendersTradingAndGitHubAppFields(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, want := range []string{
		// Trading
		`name="trading_mode"`,
		`name="trading_killSwitch"`,
		`name="trading_watchlist"`,
		`name="trading_notifyFillsChatID"`,
		// GitHubApp
		`name="githubApp_appID"`,
		`name="githubApp_privateKeyPath"`,
		`name="githubApp_installationID"`,
		`name="githubApp_apiBaseURL"`,
		`name="githubApp_webhookSecretEnv"`,
		`name="githubApp_repoAllowlist"`,
		`name="githubApp_taskLabels"`,
		`name="githubApp_prReviewLabels"`,
		`name="githubApp_senderAllowlist"`,
	} {
		assert.Contains(t, body, want, "form missing field %q", want)
	}
}

// TestProjectConfigFormSave_TradingAndGitHubAppRoundTrip —
// values round-trip through registry load after save.
func TestProjectConfigFormSave_TradingAndGitHubAppRoundTrip(t *testing.T) {
	root := writeFormFixture(t)
	server, reloader := formServer(t, root)

	form := baselineFormValues()
	// Trading
	form.Set("trading_mode", "paper")
	form.Set("trading_killSwitch", "true")
	form.Set("trading_watchlist", "AAPL\nMSFT\nNVDA")
	form.Set("trading_notifyFillsChatID", "-100123456789")
	// GitHubApp — set the full outbound triple (app_id +
	// installation_id + private_key_path) so the loader's
	// "all-or-nothing" validator is happy.
	form.Set("githubApp_appID", "555444")
	form.Set("githubApp_installationID", "987654")
	form.Set("githubApp_privateKeyPath", "/tmp/key.pem")
	form.Set("githubApp_apiBaseURL", "https://api.github.com")
	form.Set("githubApp_webhookSecretEnv", "GH_HMAC")
	form.Set("githubApp_repoAllowlist", "acme/widgets\nacme/forge")
	form.Set("githubApp_taskLabels", "todo")
	form.Set("githubApp_prReviewLabels", "needs-review")
	form.Set("githubApp_senderAllowlist", "alice\nbob")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	proj := server.projectReg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Equal(t, "paper", proj.Trading.Mode)
	assert.True(t, proj.Trading.KillSwitch)
	assert.Equal(t, []string{"AAPL", "MSFT", "NVDA"}, proj.Trading.Watchlist)
	assert.Equal(t, int64(-100123456789), proj.Trading.NotifyFillsChatID)

	assert.Equal(t, int64(555444), proj.GitHubApp.AppID)
	assert.Equal(t, int64(987654), proj.GitHubApp.InstallationID)
	assert.Equal(t, "https://api.github.com", proj.GitHubApp.APIBaseURL)
	assert.Equal(t, "GH_HMAC", proj.GitHubApp.WebhookSecretEnv)
	assert.Equal(t, []string{"acme/widgets", "acme/forge"}, proj.GitHubApp.RepoAllowlist)
	assert.Equal(t, []string{"todo"}, proj.GitHubApp.TaskLabels)
	assert.Equal(t, []string{"needs-review"}, proj.GitHubApp.PRReviewLabels)
	assert.Equal(t, []string{"alice", "bob"}, proj.GitHubApp.SenderAllowlist)
}

// TestProjectConfigFormEdit_RendersTimezoneSelect — Budget
// timezone is a select populated with the daemon's common-IANA
// list rather than a free-text input. Operators picking a
// timezone from a list shouldn't have to guess at the spelling
// of "Europe/Bratislava" vs "Europe/Vienna".
func TestProjectConfigFormEdit_RendersTimezoneSelect(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<select name="budget_timezone"`, "budget_timezone must render as <select>")
	for _, want := range []string{
		`value="UTC"`,
		`value="Europe/Prague"`,
		`value="America/New_York"`,
		`value="Asia/Tokyo"`,
	} {
		assert.Contains(t, body, want, "timezone option %q missing from dropdown", want)
	}
}

// TestProjectConfigFormEdit_RendersModelTextInputWithPricingWired —
// pricing.yaml is not a live reachability catalog, so judge_model remains
// a free-text field even when pricing is wired.
func TestProjectConfigFormEdit_RendersModelTextInputWithPricingWired(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	pricing := pricingFixture(t, "gpt-5.4", "claude-4.5-sonnet", "kimi-k2.5")
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithAssistantPricing(pricing),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<input type="text" name="judge_model"`, "judge_model must render as a text input")
	assert.NotContains(t, body, `<select name="judge_model"`)
	assert.NotContains(t, body, `value="claude-4.5-sonnet"`)
}

func TestProjectConfigFormEdit_PreservesCustomDropdownValues(t *testing.T) {
	root := writeFormFixture(t)
	custom := strings.Replace(commentedProjectYAML,
		`# budget:
#   daily_hard_usd: 20.0`,
		`budget:
  timezone: "Pacific/Chatham"
hallucinationJudge:
  enabled: true
  model: "vendor/custom-judge"
#   daily_hard_usd: 20.0`, 1)
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "form-demo.yaml"), []byte(custom), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(
		WithProjectRegistry(reg),
		WithAssistantPricing(pricingFixture(t, "gpt-5.4")),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<option value="Pacific/Chatham" selected>Pacific/Chatham</option>`)
	assert.Contains(t, body, `name="judge_model"`)
	assert.Contains(t, body, `value="vendor/custom-judge"`)
}

// TestProjectConfigFormEdit_BriefCardLinksToBriefEditor — when
// the project has a brief, the Identity section's "Brief
// attached" card must link to /brief so the operator can update
// it without leaving the form. Without this assertion the brief
// editor exists but isn't discoverable from the place where
// operators are already configuring their project.
func TestProjectConfigFormEdit_BriefCardLinksToBriefEditor(t *testing.T) {
	root := writeFormFixture(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "projects", "form-demo.md"),
		[]byte(`---
projectId: form-demo
---

## Goal

g
## Audience

a
## Success criteria

s
`),
		0o644,
	))
	server, _ := formServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `href="/ui/projects/form-demo/brief"`, "brief CTA must link to the editor when the brief exists")
}

// TestProjectConfigFormEdit_NoBriefShowsCreateCTA — when no
// brief exists, the Identity section must still surface a
// "Create brief" link so the operator can opt in. Without this
// the brief editor exists but new projects never reach it.
func TestProjectConfigFormEdit_NoBriefShowsCreateCTA(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `href="/ui/projects/form-demo/brief"`, "Create-brief CTA must be present even when no brief exists yet")
	assert.Contains(t, body, "Create brief", "Create-brief CTA copy must match the no-brief affordance")
}

// TestProjectConfigFormSave_BriefSurfacedWhenPresent — a project
// with a PROJECT.md companion renders HasBrief=true and the
// goal teaser in the form. Pins the integration between Phase
// 1A's loader and the form editor.
func TestProjectConfigFormSave_BriefSurfacedWhenPresent(t *testing.T) {
	root := writeFormFixture(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "projects", "form-demo.md"),
		[]byte(`---
projectId: form-demo
---

## Goal

Make form demos surface their briefs.

## Audience

ops

## Success criteria

renders
`),
		0o644,
	))
	server, _ := formServer(t, root)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Make form demos surface their briefs", "brief goal teaser should render")
}
