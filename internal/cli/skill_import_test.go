package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestConfigsDir lays out a minimal configs tree with one
// existing swarm so the import path has something to merge into.
func buildTestConfigsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	// Pre-seed a swarm with one existing role.
	swarmBody := `---
swarmId: my-swarm
displayName: My Swarm
roles:
  - name: lead
---

# My Swarm

## Role prompts

### lead

You are the lead.
`
	if err := os.WriteFile(filepath.Join(dir, "swarms", "my-swarm.md"), []byte(swarmBody), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	return dir
}

// runSkillImportForTest invokes runSkillImport directly with the
// caller's flag overrides. Pkg-level flag state is restored on
// defer so consecutive runs don't bleed.
func runSkillImportForTest(t *testing.T, target string, opts func()) (string, error) {
	t.Helper()
	prev := skillImportFlagsSnapshot()
	defer restoreSkillImportFlags(prev)
	skillImportConfigsDir = ""
	skillImportProject = ""
	skillImportIntoSwarm = ""
	skillImportAsSwarm = ""
	skillImportRenameWorkflow = ""
	skillImportRenameRoles = nil
	skillImportDryRun = false
	opts()

	var buf bytes.Buffer
	skillImportCmd.SetOut(&buf)
	skillImportCmd.SetErr(&buf)
	err := runSkillImport(skillImportCmd, []string{target})
	return buf.String(), err
}

type skillImportFlags struct {
	configsDir, project, intoSwarm, asSwarm, renameWorkflow string
	renameRoles                                             []string
	dryRun                                                  bool
}

func skillImportFlagsSnapshot() skillImportFlags {
	return skillImportFlags{
		configsDir:     skillImportConfigsDir,
		project:        skillImportProject,
		intoSwarm:      skillImportIntoSwarm,
		asSwarm:        skillImportAsSwarm,
		renameWorkflow: skillImportRenameWorkflow,
		renameRoles:    append([]string(nil), skillImportRenameRoles...),
		dryRun:         skillImportDryRun,
	}
}

func restoreSkillImportFlags(s skillImportFlags) {
	skillImportConfigsDir = s.configsDir
	skillImportProject = s.project
	skillImportIntoSwarm = s.intoSwarm
	skillImportAsSwarm = s.asSwarm
	skillImportRenameWorkflow = s.renameWorkflow
	skillImportRenameRoles = s.renameRoles
	skillImportDryRun = s.dryRun
}

func writeSkillFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	return path
}

const skillImportFixtureCleanRole = `---
name: research-skill
description: Research and write.
version: 1.0.0
author: vadim
license: MIT
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: research
      entrypoint: research
      steps:
        research:
          type: agent
          role: researcher
    roles:
      - name: researcher
---

# Research

## Prompts

### research

Find facts.

## Role prompts

### researcher

You are a researcher.
`

func TestSkillImport_DryRunHappyPath(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", skillImportFixtureCleanRole)

	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
		skillImportDryRun = true
	})
	if err != nil {
		t.Fatalf("dry-run import: %v\noutput=\n%s", err, out)
	}
	if !strings.Contains(out, "would write") {
		t.Errorf("expected 'would write' in dry-run output:\n%s", out)
	}
	// Dry-run must not have created the workflow file.
	if _, err := os.Stat(filepath.Join(configsDir, "workflows", "research.md")); err == nil {
		t.Errorf("dry-run wrote the workflow file")
	}
}

func TestSkillImport_RealRunWritesFiles(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", skillImportFixtureCleanRole)

	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
	})
	if err != nil {
		t.Fatalf("import: %v\noutput=\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(configsDir, "workflows", "research.md")); err != nil {
		t.Errorf("workflow file not written: %v", err)
	}
	swarmBytes, err := os.ReadFile(filepath.Join(configsDir, "swarms", "my-swarm.md"))
	if err != nil {
		t.Fatalf("read updated swarm: %v", err)
	}
	if !strings.Contains(string(swarmBytes), "researcher") {
		t.Errorf("imported researcher role not present in updated swarm:\n%s", swarmBytes)
	}
}

func TestSkillImport_RoleConflict(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	// Pre-seed swarm already has a "lead" role; build a fixture
	// whose imported role is also "lead" to force a conflict.
	body := strings.ReplaceAll(skillImportFixtureCleanRole, "researcher", "lead")
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", body)

	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
	})
	if err == nil {
		t.Fatalf("expected conflict error\noutput=\n%s", out)
	}
	if !strings.Contains(out, "already exists in target swarm") {
		t.Errorf("expected role-conflict message:\n%s", out)
	}
	// Conflict must not have written anything.
	if _, err := os.Stat(filepath.Join(configsDir, "workflows", "research.md")); err == nil {
		t.Errorf("workflow file written despite conflict")
	}
}

func TestSkillImport_RenameRoleRewritesStepRef(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	body := strings.ReplaceAll(skillImportFixtureCleanRole, "researcher", "lead")
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", body)

	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
		skillImportRenameRoles = []string{"lead=newcomer"}
	})
	if err != nil {
		t.Fatalf("rename should resolve conflict: %v\noutput=\n%s", err, out)
	}
	// Workflow file should reference the renamed role, not "lead".
	wfBytes, err := os.ReadFile(filepath.Join(configsDir, "workflows", "research.md"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if !strings.Contains(string(wfBytes), "role: newcomer") {
		t.Errorf("workflow file should reference renamed role:\n%s", wfBytes)
	}
}

func TestSkillImport_AsSwarmCreatesNewFile(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", skillImportFixtureCleanRole)

	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportAsSwarm = "brand-new"
	})
	if err != nil {
		t.Fatalf("--as-swarm import: %v\noutput=\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(configsDir, "swarms", "brand-new.md")); err != nil {
		t.Errorf("new swarm file not created: %v", err)
	}
}

func TestSkillImport_NoTargetSwarm(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", skillImportFixtureCleanRole)
	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
	})
	if err == nil || !strings.Contains(err.Error(), "specify a target swarm") {
		t.Errorf("expected target-swarm error, got %v\noutput=\n%s", err, out)
	}
}

func TestSkillImport_RejectsInvalidFile(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", "---\nname: BadName\n---\n")
	out, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
	})
	if err == nil {
		t.Errorf("invalid skill should be rejected\noutput=\n%s", out)
	}
}

func TestSkillImport_RejectsUnsafeWorkflowID(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	body := strings.Replace(skillImportFixtureCleanRole, "workflowId: research", "workflowId: ../escape", 1)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", body)
	_, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportIntoSwarm = "my-swarm"
	})
	if err == nil || !strings.Contains(err.Error(), "workflowId") {
		t.Fatalf("expected unsafe workflowId rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(configsDir, "escape.md")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe workflow wrote outside workflows dir")
	}
}

func TestSkillImport_RejectsUnsafeSwarmFlag(t *testing.T) {
	configsDir := buildTestConfigsDir(t)
	dir := t.TempDir()
	path := writeSkillFile(t, dir, "skill.md", skillImportFixtureCleanRole)
	_, err := runSkillImportForTest(t, path, func() {
		skillImportConfigsDir = configsDir
		skillImportAsSwarm = "../escape"
	})
	if err == nil || !strings.Contains(err.Error(), "swarmId") {
		t.Fatalf("expected unsafe swarmId rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(configsDir, "escape.md")); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe swarm wrote outside swarms dir")
	}
}
