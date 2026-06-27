package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const skillCleanFixture = `---
name: cli-demo
description: Tiny valid skill used by the CLI's own tests.
version: 1.0.0
author: vornik
license: Apache-2.0
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: cli-demo
      entrypoint: only
      steps:
        only:
          type: agent
          role: lead
    roles:
      - name: lead
---

# CLI demo

## Prompts

### only

Do the thing.

## Role prompts

### lead

You are the lead.
`

func runSkillValidateForTest(t *testing.T, target string, asJSON bool) (string, error) {
	t.Helper()
	skillValidateFix = false
	skillValidateJSON = asJSON
	defer func() { skillValidateJSON = false }()

	var buf bytes.Buffer
	skillValidateCmd.SetOut(&buf)
	skillValidateCmd.SetErr(&buf)
	err := runSkillValidate(skillValidateCmd, []string{target})
	return buf.String(), err
}

func writeTempSkill(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestSkillValidateCmd_AcceptsCleanFile(t *testing.T) {
	path := writeTempSkill(t, skillCleanFixture)
	out, err := runSkillValidateForTest(t, path, false)
	if err != nil {
		t.Fatalf("clean fixture should pass: err=%v\noutput=\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("clean output missing OK line:\n%s", out)
	}
}

func TestSkillValidateCmd_FailsOnBadName(t *testing.T) {
	body := strings.Replace(skillCleanFixture, "name: cli-demo", "name: Bad_Name", 1)
	path := writeTempSkill(t, body)
	out, err := runSkillValidateForTest(t, path, false)
	if err == nil {
		t.Fatalf("bad name should fail; got no error\noutput=\n%s", out)
	}
	if !strings.Contains(out, "name_shape") {
		t.Errorf("expected name_shape finding:\n%s", out)
	}
}

func TestSkillValidateCmd_DirectoryDispatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(skillCleanFixture), 0o644); err != nil {
		t.Fatalf("write a.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte(skillCleanFixture), 0o644); err != nil {
		t.Fatalf("write b.md: %v", err)
	}
	out, err := runSkillValidateForTest(t, dir, false)
	if err != nil {
		t.Fatalf("clean dir should pass: %v\noutput=\n%s", err, out)
	}
	if !strings.Contains(out, "2 file(s) checked") {
		t.Errorf("expected summary line for 2 files:\n%s", out)
	}
}

func TestSkillValidateCmd_EmptyDirIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := runSkillValidateForTest(t, dir, false)
	if err == nil {
		t.Errorf("empty dir should error so the operator notices their typo")
	}
}

func TestSkillValidateCmd_MissingFile(t *testing.T) {
	_, err := runSkillValidateForTest(t, "/nonexistent/missing.md", false)
	if err == nil {
		t.Errorf("missing file should error")
	}
}

func TestSkillValidateCmd_JSONOutput(t *testing.T) {
	path := writeTempSkill(t, skillCleanFixture)
	out, err := runSkillValidateForTest(t, path, true)
	if err != nil {
		t.Fatalf("clean fixture json: %v", err)
	}
	if !strings.Contains(out, "\"path\"") || !strings.Contains(out, "\"report\"") {
		t.Errorf("expected json envelope:\n%s", out)
	}
}
