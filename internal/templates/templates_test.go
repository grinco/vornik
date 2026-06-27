package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// writeManifest writes a minimal manifest + one source template
// into a per-test temp directory. Keeps the per-test setup short
// so behaviour assertions stay readable.
func writeManifest(t *testing.T, dir, slug, manifest, sourceName, sourceBody string) {
	t.Helper()
	d := filepath.Join(dir, slug)
	require.NoError(t, os.MkdirAll(d, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d, "template.yaml"), []byte(manifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(d, sourceName), []byte(sourceBody), 0o644))
}

func TestLoad_EmptyRootIsNotAnError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "missing root must not error — deployments without templates installed should still run")
	require.NotNil(t, c)
	assert.Empty(t, c.List())
}

func TestLoad_HappyPath_ParsesAllManifests(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "alpha", `
displayName: "Alpha"
description: "A test template."
domain: "general"
parameters:
  - {name: projectId, type: string, label: "ID", pattern: "[a-z]+", required: true}
files:
  - {source: out.tmpl, target: "projects/{{.projectId}}.yaml"}
`, "out.tmpl", "projectId: {{.projectId}}\n")
	writeManifest(t, dir, "beta", `
displayName: "Beta"
description: "Another test template."
domain: "research"
parameters:
  - {name: name, type: string, label: "Name", default: "Beta"}
files:
  - {source: out.tmpl, target: "projects/beta.yaml"}
`, "out.tmpl", "name: {{.name}}\n")

	c, err := Load(dir)
	require.NoError(t, err)
	list := c.List()
	require.Len(t, list, 2)
	// List orders by (domain, displayName) — "general"<"research".
	assert.Equal(t, "alpha", list[0].Slug)
	assert.Equal(t, "beta", list[1].Slug)
	// Default domain fallback when unset is "general".
	got, ok := c.Get("alpha")
	require.True(t, ok)
	assert.Equal(t, "general", got.Domain)
}

func TestLoad_SkipsDirectoriesWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "work-in-progress"), 0o755))
	// And one valid template alongside.
	writeManifest(t, dir, "good", `
displayName: "Good"
parameters: []
files:
  - {source: out.tmpl, target: "projects/good.yaml"}
`, "out.tmpl", "ok\n")

	c, err := Load(dir)
	require.NoError(t, err, "a directory without template.yaml must not error — partial templates on disk are normal during dev")
	assert.Len(t, c.List(), 1)
}

func TestLoad_RejectsMalformedManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "bad", `displayName: "" # required
parameters: []
files:
  - {source: out.tmpl, target: "projects/x.yaml"}
`, "out.tmpl", "x\n")
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "displayName is required")
}

func TestLoad_RejectsDuplicateParameterName(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "dup", `
displayName: "Dup"
parameters:
  - {name: x, type: string, label: "X"}
  - {name: x, type: string, label: "X again"}
files:
  - {source: out.tmpl, target: "projects/x.yaml"}
`, "out.tmpl", "x\n")
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate parameter")
}

func TestLoad_RejectsEnumWithoutOptions(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "enum-bad", `
displayName: "Enum"
parameters:
  - {name: pick, type: enum, label: "Pick"}
files:
  - {source: out.tmpl, target: "projects/x.yaml"}
`, "out.tmpl", "x\n")
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enum")
}

func TestLoad_RejectsTraversalSource(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "evil", `
displayName: "Evil"
parameters: []
files:
  - {source: "../etc/passwd", target: "projects/x.yaml"}
`, "out.tmpl", "x\n")
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent-dir traversal")
}

func TestLoad_RejectsAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "evil", `
displayName: "Evil"
parameters: []
files:
  - {source: out.tmpl, target: "/tmp/escaped.yaml"}
`, "out.tmpl", "x\n")
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestValidateParams_HappyPathFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "ok", `
displayName: "OK"
parameters:
  - {name: x, type: string, label: "X", default: "default-x"}
  - {name: y, type: string, label: "Y", pattern: "[a-z]+", required: true}
files:
  - {source: out.tmpl, target: "projects/{{.x}}.yaml"}
`, "out.tmpl", "x={{.x}} y={{.y}}\n")
	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("ok")

	got, err := ValidateParams(m, map[string]string{"y": "hello"})
	require.NoError(t, err)
	assert.Equal(t, "default-x", got["x"], "missing optional must take Default")
	assert.Equal(t, "hello", got["y"])
}

func TestValidateParams_RequiredMissing(t *testing.T) {
	m := Manifest{
		DisplayName: "X",
		Parameters: []Parameter{
			{Name: "id", Type: "string", Required: true},
		},
		Files: []FileMap{{Source: "x", Target: "y"}},
	}
	_, err := ValidateParams(m, map[string]string{})
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "id", ve.Field)
	assert.Contains(t, ve.Message, "required")
}

func TestValidateParams_PatternMismatch(t *testing.T) {
	m := Manifest{
		DisplayName: "X",
		Parameters: []Parameter{
			{Name: "id", Type: "string", Pattern: "[a-z]+"},
		},
		Files: []FileMap{{Source: "x", Target: "y"}},
	}
	_, err := ValidateParams(m, map[string]string{"id": "UPPER-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must match pattern")
}

func TestValidateParams_EnumOutOfList(t *testing.T) {
	m := Manifest{
		DisplayName: "X",
		Parameters: []Parameter{
			{Name: "size", Type: "enum", Options: []string{"S", "M", "L"}, Required: true},
		},
		Files: []FileMap{{Source: "x", Target: "y"}},
	}
	_, err := ValidateParams(m, map[string]string{"size": "XL"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be one of")
}

func TestValidateParams_UnknownParamRejected(t *testing.T) {
	m := Manifest{
		DisplayName: "X",
		Parameters: []Parameter{
			{Name: "id", Type: "string", Default: "x"},
		},
		Files: []FileMap{{Source: "x", Target: "y"}},
	}
	_, err := ValidateParams(m, map[string]string{"sneaky": "value"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown parameter")
}

func TestMaterialiseFiles_RendersBothPathAndBody(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "render", `
displayName: "Render"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z]+"}
  - {name: greeting, type: string, label: "Greet", default: "hello"}
files:
  - {source: project.yaml.tmpl, target: "projects/{{.projectId}}.yaml"}
`, "project.yaml.tmpl", "id: {{.projectId}}\ngreeting: {{.greeting}}\n")

	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("render")
	out, err := c.MaterialiseFiles(m, map[string]string{"projectId": "alpha"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	body, ok := out["projects/alpha.yaml"]
	require.True(t, ok, "target path must be rendered: %v", out)
	assert.Equal(t, "id: alpha\ngreeting: hello\n", body)
}

func TestMaterialiseFiles_RefusesTraversalInRenderedTarget(t *testing.T) {
	dir := t.TempDir()
	// Manifest passes static validation (target has no literal "..").
	// But the parameter value tries to inject one.
	writeManifest(t, dir, "trav", `
displayName: "Trav"
parameters:
  - {name: path, type: string, label: "Path"}
files:
  - {source: out.tmpl, target: "{{.path}}/x.yaml"}
`, "out.tmpl", "x\n")

	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("trav")
	_, err = c.MaterialiseFiles(m, map[string]string{"path": "../evil"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent-dir traversal")
}

func TestMaterialiseFiles_RefusesAbsoluteRenderedTarget(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "abs", `
displayName: "Abs"
parameters:
  - {name: path, type: string, label: "Path"}
files:
  - {source: out.tmpl, target: "{{.path}}/x.yaml"}
`, "out.tmpl", "x\n")

	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("abs")
	_, err = c.MaterialiseFiles(m, map[string]string{"path": "/tmp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute")
}

func TestMaterialiseFiles_TemplateMissingKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "missing", `
displayName: "Missing"
parameters:
  - {name: known, type: string, default: "x"}
files:
  - {source: out.tmpl, target: "out.yaml"}
`, "out.tmpl", "{{.unknown}}\n")

	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("missing")
	_, err = c.MaterialiseFiles(m, nil)
	require.Error(t, err, "template referencing an undeclared key must error rather than render empty — protects against typos in author-time templates")
	assert.True(t, strings.Contains(err.Error(), "unknown") || strings.Contains(err.Error(), "missingkey"))
}

func TestMaterialiseFiles_RefusesSymlinkedSourceEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.tmpl")
	require.NoError(t, os.WriteFile(outside, []byte("stolen\n"), 0o644))
	writeManifest(t, dir, "symlink-src", `
displayName: "Symlink"
parameters: []
files:
  - {source: out.tmpl, target: "projects/x.yaml"}
`, "out.tmpl", "safe\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "symlink-src", "out.tmpl")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "symlink-src", "out.tmpl")))

	c, err := Load(dir)
	require.NoError(t, err)
	m, _ := c.Get("symlink-src")
	_, err = c.MaterialiseFiles(m, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
}

func TestWriteRenderedFilesExclusive_RefusesTraversalAndSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	_, err := WriteRenderedFilesExclusive(root, map[string]string{"../escape.yaml": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent-dir traversal")

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "linked")))
	_, err = WriteRenderedFilesExclusive(root, map[string]string{"linked/escape.yaml": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")
	if _, statErr := os.Stat(filepath.Join(outside, "escape.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("write escaped through symlink; stat err=%v", statErr)
	}
}

func TestSortedTargets_DeterministicOrder(t *testing.T) {
	got := SortedTargets(map[string]string{
		"swarms/z.yaml":    "",
		"projects/a.yaml":  "",
		"workflows/m.yaml": "",
	})
	require.Equal(t, []string{"projects/a.yaml", "swarms/z.yaml", "workflows/m.yaml"}, got)
}

// TestShippedTemplatesParse — every template that ships with the
// daemon must Load() without error. Catches regressions where an
// author edits a manifest in a way that fails the validator (bad
// regex, missing required field) before a release goes out.
func TestShippedTemplatesParse(t *testing.T) {
	root := filepath.Join("..", "..", "configs", "project-templates")
	// If we're running from a non-source build (CI image without
	// the configs tree mounted), the directory is absent — that's
	// not a failure, just skip.
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Skipf("shipped templates dir not present at %s", root)
	}
	c, err := Load(root)
	require.NoError(t, err, "every shipped template must Load cleanly — a release-blocking failure")
	list := c.List()
	require.NotEmpty(t, list, "ship at least one template")

	// Sanity-check materialisation for every shipped template that
	// renders a swarm file. The 2026-05-17 SWARM.md migration made
	// the registry .md-only; the templates that still rendered
	// .yaml swarm files were silently ignored, producing
	// "registered project pointing at a non-existent swarm" — the
	// bug discovered on 2026-05-19. We catch any reintroduction by:
	//   1) requiring the swarm target to end in .md
	//   2) parsing the rendered body through ParseSwarmMarkdown
	//      itself, so structural mistakes (missing swarmId,
	//      `### role` heading with no matching role, etc.) fail
	//      the test rather than the next operator's daemon load.
	pa, ok := c.Get("personal-assistant")
	require.True(t, ok, "personal-assistant template must be present")
	paOut, merr := c.MaterialiseFiles(pa, map[string]string{
		"projectId":   "my-assistant",
		"displayName": "My Assistant",
		"llmModel":    "openai.gpt-oss-120b-1:0",
	})
	require.NoError(t, merr, "personal-assistant must materialise with the canonical parameter set")
	assert.Contains(t, paOut, "projects/my-assistant.yaml")
	assert.Contains(t, paOut, "swarms/my-assistant-swarm.md")
	assert.NotContains(t, paOut, "swarms/my-assistant-swarm.yaml",
		"swarm file must be .md so the SWARM.md registry loader picks it up")
	assert.Contains(t, paOut["projects/my-assistant.yaml"], "projectId: \"my-assistant\"")
	swarm, perr := registry.ParseSwarmMarkdown(
		[]byte(paOut["swarms/my-assistant-swarm.md"]),
		"my-assistant-swarm.md",
	)
	require.NoError(t, perr, "rendered SWARM.md must parse")
	assert.Equal(t, "my-assistant-swarm", swarm.ID,
		"frontmatter must use swarmId key (yaml struct tag), not id")

	// Same gate for news-feed — also rendered a .yaml swarm pre-fix.
	if nf, ok := c.Get("news-feed"); ok {
		nfOut, nferr := c.MaterialiseFiles(nf, map[string]string{
			"projectId":   "my-feed",
			"displayName": "My Feed",
			"topic":       "kubernetes ecosystem",
			"interval":    "4h",
			"llmModel":    "openai.gpt-oss-120b-1:0",
		})
		require.NoError(t, nferr, "news-feed must materialise with the canonical parameter set")
		assert.Contains(t, nfOut, "swarms/my-feed-swarm.md")
		assert.NotContains(t, nfOut, "swarms/my-feed-swarm.yaml")
		_, nfParse := registry.ParseSwarmMarkdown(
			[]byte(nfOut["swarms/my-feed-swarm.md"]),
			"my-feed-swarm.md",
		)
		require.NoError(t, nfParse, "news-feed rendered SWARM.md must parse")

		// Cross-validation: a materialised template MUST end up
		// loading cleanly through the same registry path the
		// daemon uses, alongside the shipped workflows. The
		// 2026-05-19 third-bug was a missing `lead` role in the
		// starter swarm — the .md format fix alone wasn't enough;
		// the project's default `adaptive` workflow references
		// `lead`. Parse-in-isolation didn't catch it, but a
		// full registry.Load against a hydrated temp dir does.
		hydrateAndValidate(t, nfOut, filepath.Join("..", "..", "configs", "workflows"))
	}

	// Same cross-validation gate for personal-assistant. We do it
	// after both template sub-tests so a single helper call
	// covers everything the templates dropped on disk.
	hydrateAndValidate(t, paOut, filepath.Join("..", "..", "configs", "workflows"))

	// Companion template (LLD 21). Materialises the per-user
	// async-offload project the vornik-companion plugin contract
	// depends on. The hydrate-and-validate gate catches any drift
	// between the swarm.md.tmpl's roles (reviewer/analyst/summarizer)
	// and the role names referenced by configs/workflows/companion-*.
	if co, ok := c.Get("companion"); ok {
		coOut, coerr := c.MaterialiseFiles(co, map[string]string{
			"projectId":    "companion-tester",
			"displayName":  "Companion Tester",
			"defaultModel": "openai.gpt-oss-120b-1:0",
		})
		require.NoError(t, coerr, "companion template must materialise with the canonical parameter set")
		assert.Contains(t, coOut, "projects/companion-tester.yaml")
		assert.Contains(t, coOut, "swarms/companion-tester-swarm.md")
		assert.NotContains(t, coOut, "swarms/companion-tester-swarm.yaml",
			"swarm file must be .md so the SWARM.md registry loader picks it up")
		// Spot-check the rendered project actually pins the
		// six companion workflows as adaptive candidates — if a
		// future editor drops one, the host LLM client loses a
		// delegation surface and the contract silently shrinks.
		body := coOut["projects/companion-tester.yaml"]
		for _, wfID := range []string{
			"companion-architectural-review",
			"companion-test-coverage-audit",
			"companion-doc-review",
			"companion-data-validation",
			"companion-research-gather",
			"companion-report-summarize",
		} {
			assert.Containsf(t, body, wfID,
				"companion project.yaml.tmpl must list %s as a candidate workflow", wfID)
		}
		swarm, perr := registry.ParseSwarmMarkdown(
			[]byte(coOut["swarms/companion-tester-swarm.md"]),
			"companion-tester-swarm.md",
		)
		require.NoError(t, perr, "rendered companion SWARM.md must parse")
		assert.Equal(t, "companion-tester-swarm", swarm.ID)
		// All three roles must be present — every shipped
		// companion-* workflow pins one of them by name; missing
		// any role makes the corresponding workflow unrunnable.
		roleNames := map[string]bool{}
		for _, role := range swarm.Roles {
			roleNames[role.Name] = true
		}
		for _, expected := range []string{"reviewer", "analyst", "summarizer"} {
			assert.Truef(t, roleNames[expected],
				"companion swarm must include role %q (referenced by companion-* workflows)", expected)
		}
		hydrateAndValidate(t, coOut, filepath.Join("..", "..", "configs", "workflows"))
	} else {
		t.Errorf("companion template not present under configs/project-templates/companion — LLD 21 regressed?")
	}
}

// hydrateAndValidate writes a templates-rendered map onto a
// throwaway directory, copies the shipped workflows in, then runs
// the same registry.Load the daemon uses at boot. A clean Load
// means the materialised project + swarm + shipped workflows
// form a consistent set. Used by TestShippedTemplatesParse so any
// future swarm-role / workflow-role drift fails at test time
// rather than silently producing a registered-but-unrunnable
// project (the operator-visible symptom on 2026-05-19).
func hydrateAndValidate(t *testing.T, rendered map[string]string, workflowsSrc string) {
	t.Helper()
	root := t.TempDir()
	for path, body := range rendered {
		full := filepath.Join(root, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	// Copy shipped workflows into the temp tree so the registry
	// can resolve project.defaultWorkflowId and the candidate
	// list. Single-file copies keep the test hermetic — any
	// unrelated workflow on disk can't accidentally satisfy a
	// reference the template's project depends on.
	entries, err := os.ReadDir(workflowsSrc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(workflowsSrc, e.Name()))
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", e.Name()), body, 0o644))
	}
	reg := registry.New()
	if loadErr := reg.Load(root); loadErr != nil {
		t.Fatalf("template-materialised registry must Load cleanly: %v", loadErr)
	}
	// A successful Load can still strip invalid projects with a
	// warning rather than fail outright. Assert the loaded
	// projects include every project file we dropped — if any
	// got stripped, the materialised template is producing an
	// invalid project (e.g. role-not-in-swarm, like the
	// 2026-05-19 third bug).
	loaded := reg.ListProjects()
	loadedIDs := make(map[string]bool, len(loaded))
	for _, p := range loaded {
		loadedIDs[p.ID] = true
	}
	for path := range rendered {
		if !strings.HasPrefix(path, "projects/") {
			continue
		}
		// Expected ID is the YAML's projectId, but the simpler
		// invariant is filename-derived: <id>.yaml maps to <id>.
		base := strings.TrimSuffix(filepath.Base(path), ".yaml")
		assert.Truef(t, loadedIDs[base],
			"project %s did not load — likely a role reference in a candidate workflow points at a role not present in the starter swarm", base)
	}
}

// TestCatalog_DomainsReturnsSortedDistinct anchors the tab-strip
// data contract: distinct domain values, alphabetically ordered.
// The gallery UI iterates this list to render the filter tabs.
func TestCatalog_DomainsReturnsSortedDistinct(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "a", `
displayName: "A"
domain: "research"
files:
  - {source: out.tmpl, target: "p/a.yaml"}
`, "out.tmpl", "x")
	writeManifest(t, dir, "b", `
displayName: "B"
domain: "general"
files:
  - {source: out.tmpl, target: "p/b.yaml"}
`, "out.tmpl", "x")
	writeManifest(t, dir, "c", `
displayName: "C"
domain: "research"
files:
  - {source: out.tmpl, target: "p/c.yaml"}
`, "out.tmpl", "x")

	c, err := Load(dir)
	require.NoError(t, err)
	got := c.Domains()
	assert.Equal(t, []string{"general", "research"}, got,
		"Domains() must return distinct values, alphabetical so the tab strip is stable across reloads")
}

// TestCatalog_DomainsNilCatalog — defensive: nil receiver returns
// nil (matches Catalog.List behaviour) so callers don't need a
// nil-guard before iterating.
func TestCatalog_DomainsNilCatalog(t *testing.T) {
	var c *Catalog
	assert.Nil(t, c.Domains())
}

// TestCatalog_DomainsEmptyCatalog — a freshly-loaded catalog with
// no entries returns an empty (not nil) slice; callers can range
// over it safely.
func TestCatalog_DomainsEmptyCatalog(t *testing.T) {
	c, err := Load(t.TempDir())
	require.NoError(t, err)
	got := c.Domains()
	assert.Empty(t, got)
}
