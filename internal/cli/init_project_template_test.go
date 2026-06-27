package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDemoTemplate creates a minimal valid template under
// <configDir>/project-templates/demo plus the supporting swarm +
// workflow the rendered project references — so the rendered set
// loads cleanly through registry.Load(). Returns the configDir so
// tests can assert file outputs.
//
// Pre-2026-05-27 this only wrote the template; the rendered project
// had no SwarmID + no DefaultWorkflowID and would have failed real
// registry loading, but the init path didn't validate. The
// companion-onboarding fix added pre-write validation, which means
// this fixture must now stand up a complete registry.
func writeDemoTemplate(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	// Supporting swarm + workflow the demo project references.
	// Drop them in the canonical registry locations so
	// validateRenderedTemplate's hydrate step finds them.
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "demo-swarm.md"), []byte(`---
swarmId: "demo-swarm"
roles:
  - name: "worker"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", "demo-wf.md"), []byte(`---
workflowId: "demo-wf"
entrypoint: "step1"
steps:
  step1:
    type: "agent"
    role: "worker"
    prompt: "test"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))
	tplDir := filepath.Join(configDir, "project-templates", "demo")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "template.yaml"), []byte(`
displayName: "Demo CLI"
description: "test"
category: "general"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
  - {name: greeting, type: string, label: "Greet", default: "hello"}
files:
  - {source: project.yaml.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "project.yaml.tmpl"),
		[]byte(`projectId: {{.projectId}}
displayName: "{{.projectId}}"
swarmId: "demo-swarm"
defaultWorkflowId: "demo-wf"
# greeting is a test-only param the registry ignores.
`), 0o644))
	return configDir
}

// resetTemplateFlags restores the package-level vars between
// tests. Cobra wires flags into globals (`initTemplate`,
// `initParams`, etc.) so a previous test's values would leak
// otherwise.
func resetTemplateFlags() {
	initTemplate = ""
	initParams = nil
	initDryRun = false
	initForce = false
	initConfigDir = ""
}

func TestParseParamFlags_HappyPath(t *testing.T) {
	got, err := parseParamFlags([]string{"a=1", "b=hello world", "c=eq=in=value"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "hello world", "c": "eq=in=value"}, got,
		"= in the value must be preserved — only the FIRST = is the delimiter")
}

func TestParseParamFlags_RejectsMalformed(t *testing.T) {
	cases := []string{"no-equals", "=value-without-name", ""}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := parseParamFlags([]string{c})
			require.Error(t, err)
		})
	}
}

func TestRunInitProjectFromTemplate_HappyPath_MaterialisesFiles(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	initParams = []string{"projectId=my-proj", "greeting=hi"}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, runInitProjectFromTemplate(cmd, []string{"my-proj"}, configDir))

	got, err := os.ReadFile(filepath.Join(configDir, "projects", "my-proj.yaml"))
	require.NoError(t, err, "template materialisation must write the rendered file")
	body := string(got)
	assert.Contains(t, body, "projectId: my-proj")
	assert.Contains(t, body, `swarmId: "demo-swarm"`)
	assert.Contains(t, body, `defaultWorkflowId: "demo-wf"`)
}

func TestRunInitProjectFromTemplate_FallsBackToPositionalProjectID(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	// No --param projectId=... — the positional arg fills it in.
	initParams = nil
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, runInitProjectFromTemplate(cmd, []string{"My Project"}, configDir),
		"positional name must be sanitised + used as projectId when --param doesn't set it")

	_, err := os.Stat(filepath.Join(configDir, "projects", "my-project.yaml"))
	require.NoError(t, err, "sanitised positional projectId must drive the rendered target path")
}

// TestRunInitProjectFromTemplate_RejectsWhenWorkflowMissing is the
// regression for the 2026-05-27 companion-onboarding bug. Before the
// fix, a template that materialised a project referencing a workflow
// that isn't on disk yet would write files cleanly and then the
// project would be silently stripped on the next config reload. The
// CLI must now fail BEFORE touching disk, with the missing-workflow
// name in the error message so the operator can self-correct.
func TestRunInitProjectFromTemplate_RejectsWhenWorkflowMissing(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := t.TempDir()
	// Swarm present, workflow MISSING — the exact shape the companion
	// onboarding hit when 'make install' didn't ship companion-*.md.
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "tester-swarm.md"), []byte(`---
swarmId: "tester-swarm"
roles:
  - name: "worker"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))
	tplDir := filepath.Join(configDir, "project-templates", "missing-wf")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "template.yaml"), []byte(`
displayName: "Missing-WF template"
description: "references a workflow that isn't on disk"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
files:
  - {source: project.yaml.tmpl, target: "projects/{{.projectId}}.yaml"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "project.yaml.tmpl"), []byte(`projectId: {{.projectId}}
displayName: "{{.projectId}}"
swarmId: "tester-swarm"
defaultWorkflowId: "this-workflow-was-never-deployed"
`), 0o644))

	initTemplate = "missing-wf"
	initParams = []string{"projectId=should-fail"}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	err := runInitProjectFromTemplate(cmd, []string{"should-fail"}, configDir)
	require.Error(t, err,
		"missing workflow must surface BEFORE any file is written")
	assert.Contains(t, err.Error(), "this-workflow-was-never-deployed",
		"error must name the missing workflow so the operator can self-correct")

	// And confirm no file leaked onto disk.
	_, statErr := os.Stat(filepath.Join(configDir, "projects", "should-fail.yaml"))
	assert.True(t, os.IsNotExist(statErr),
		"failed validation must not leave a half-written project on disk")
}

func TestRunInitProjectFromTemplate_UnknownTemplateLists(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "absent"
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	err := runInitProjectFromTemplate(cmd, []string{"x"}, configDir)
	require.Error(t, err)
	// Must list the available templates so the operator can fix
	// the flag without diving into config trees.
	assert.Contains(t, err.Error(), "demo",
		"error message must list installed templates so the operator can self-correct")
}

func TestRunInitProjectFromTemplate_DryRunWritesNothing(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	initParams = []string{"projectId=dry-test"}
	initDryRun = true

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(buf)
	require.NoError(t, runInitProjectFromTemplate(cmd, []string{"dry-test"}, configDir))

	// Output contains the rendered preview.
	assert.Contains(t, buf.String(), "projectId: dry-test")
	// But no file got written.
	_, err := os.Stat(filepath.Join(configDir, "projects", "dry-test.yaml"))
	assert.True(t, os.IsNotExist(err), "--dry-run must NOT touch the filesystem")
}

func TestRunInitProjectFromTemplate_RefusesExistingFileWithoutForce(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	initParams = []string{"projectId=collide"}

	// Pre-place the target.
	targetDir := filepath.Join(configDir, "projects")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "collide.yaml"),
		[]byte("existing\n"), 0o644))

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	err := runInitProjectFromTemplate(cmd, []string{"collide"}, configDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Pre-existing content unchanged.
	got, _ := os.ReadFile(filepath.Join(targetDir, "collide.yaml"))
	assert.Equal(t, "existing\n", string(got),
		"refused collision must NOT mutate the existing file")
}

func TestRunInitProjectFromTemplate_ForceAllowsOverwrite(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	initParams = []string{"projectId=collide"}
	initForce = true

	targetDir := filepath.Join(configDir, "projects")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "collide.yaml"),
		[]byte("old\n"), 0o644))

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, runInitProjectFromTemplate(cmd, []string{"collide"}, configDir))

	got, _ := os.ReadFile(filepath.Join(targetDir, "collide.yaml"))
	assert.True(t, strings.Contains(string(got), "projectId: collide"),
		"--force must overwrite with the freshly-rendered content")
}

func TestRunInitProjectFromTemplate_InvalidParameterRejected(t *testing.T) {
	t.Cleanup(resetTemplateFlags)
	configDir := writeDemoTemplate(t)
	initTemplate = "demo"
	// projectId pattern is lowercase+hyphens; uppercase fails.
	initParams = []string{"projectId=INVALID"}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	err := runInitProjectFromTemplate(cmd, []string{"x"}, configDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pattern",
		"pattern-validation failures surface to the operator with the specific rule that failed")
}
