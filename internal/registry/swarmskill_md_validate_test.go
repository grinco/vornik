package registry

import (
	"strings"
	"testing"
)

// validReport returns a SwarmSkill bytes blob the validator
// should accept with zero ERROR findings (warnings allowed).
func validSkillBytes(t *testing.T) []byte {
	t.Helper()
	skill := makeTestSkill()
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func TestValidateSwarmSkillMarkdown_HappyPath(t *testing.T) {
	report := ValidateSwarmSkillMarkdown(validSkillBytes(t), "happy.md")
	if report.HasErrors() {
		t.Errorf("happy path produced errors: %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_NameRules(t *testing.T) {
	cases := []struct {
		name      string
		nameField string
		wantCode  string
	}{
		{"missing", "", "name_missing"},
		{"too long", strings.Repeat("a", swarmSkillNameMaxLen+1) + "-x", "name_too_long"},
		{"shape underscore", "bad_name", "name_shape"},
		{"shape uppercase", "BadName", "name_shape"},
		{"shape leading hyphen", "-bad", "name_shape"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := makeSkillBytesWithName(t, tc.nameField)
			report := ValidateSwarmSkillMarkdown(content, "name.md")
			if !hasCode(report.Findings, tc.wantCode) {
				t.Errorf("want %s in findings, got %v", tc.wantCode, findingCodes(report.Findings))
			}
		})
	}
}

func TestValidateSwarmSkillMarkdown_VersionShape(t *testing.T) {
	content := makeSkillBytesWithVersion(t, "not-a-version")
	report := ValidateSwarmSkillMarkdown(content, "ver.md")
	if !hasCode(report.Findings, "version_shape") {
		t.Errorf("want version_shape in findings, got %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_AuthorLicenseWarn(t *testing.T) {
	skill := makeTestSkill()
	skill.Author = ""
	skill.License = ""
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	report := ValidateSwarmSkillMarkdown(out, "nl.md")
	if report.HasErrors() {
		t.Errorf("missing author+license should be warnings only, got errors: %v", findingCodes(report.Findings))
	}
	if !hasCode(report.Findings, "author_missing") {
		t.Errorf("want author_missing warning")
	}
	if !hasCode(report.Findings, "license_missing") {
		t.Errorf("want license_missing warning")
	}
}

func TestValidateSwarmSkillMarkdown_StandardFileWarns(t *testing.T) {
	skill := makeTestSkill()
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{Standard: true})
	if err != nil {
		t.Fatalf("marshal standard: %v", err)
	}
	// Standard files have body prompt sections referencing
	// missing structural data — the parser refuses them, so the
	// validator surfaces a single 'parse' finding.
	report := ValidateSwarmSkillMarkdown(out, "std.md")
	if !report.HasErrors() {
		t.Errorf("expected parse error for standard file, got: %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_StandardFileWithoutPromptsWarns(t *testing.T) {
	// A genuinely standard file (no body prompt sections) parses
	// cleanly; the validator emits vornik_payload_missing as a
	// warning so the operator knows non-vornik consumers will
	// see only the prose.
	content := `---
name: clean-standard
description: A clean standard file with no body sections.
version: 1.0.0
author: me
license: MIT
---

# Clean

Just prose, no metadata.vornik block.
`
	report := ValidateSwarmSkillMarkdown([]byte(content), "clean.md")
	if report.HasErrors() {
		t.Errorf("clean standard file should not error, got: %v", findingCodes(report.Findings))
	}
	if !hasCode(report.Findings, "vornik_payload_missing") {
		t.Errorf("want vornik_payload_missing warning, got %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_WorkflowIDMismatch(t *testing.T) {
	skill := makeTestSkill()
	skill.Workflow.ID = "totally-different-id"
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	report := ValidateSwarmSkillMarkdown(out, "mismatch.md")
	if report.HasErrors() {
		t.Errorf("workflowId mismatch is a warning, not an error: %v", findingCodes(report.Findings))
	}
	if !hasCode(report.Findings, "workflow_id_mismatch") {
		t.Errorf("want workflow_id_mismatch in findings, got %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_SchemaVersionUnsupported(t *testing.T) {
	content := `---
name: foo
description: d with body content
version: 1.0.0
author: me
license: MIT
metadata:
  vornik:
    schema_version: 99
---
`
	report := ValidateSwarmSkillMarkdown([]byte(content), "ver99.md")
	if !report.HasErrors() {
		t.Errorf("schema_version 99 must be an error, got %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_RoleUnreferenced(t *testing.T) {
	skill := makeTestSkill()
	skill.Roles = append(skill.Roles, SwarmRole{Name: "lonely", SystemPrompt: "hi"})
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	report := ValidateSwarmSkillMarkdown(out, "lonely.md")
	if !hasCode(report.Findings, "role_unreferenced") {
		t.Errorf("want role_unreferenced warning for 'lonely', got %v", findingCodes(report.Findings))
	}
}

func TestValidateSwarmSkillMarkdown_FileSizeSoft(t *testing.T) {
	// Build a file with > soft cap chars by padding the description body.
	// Description hard-cap is 1024 so we pad with multiple roles each
	// with a long-ish system prompt; the sum exceeds the soft cap.
	skill := makeTestSkill()
	pad := strings.Repeat("x", 4_000)
	skill.Roles[0].SystemPrompt = pad
	skill.Roles[1].SystemPrompt = pad
	skill.Workflow.Steps["research"] = WorkflowStep{
		Type:      "agent",
		Role:      "researcher",
		Prompt:    pad,
		OnSuccess: "write",
		OnFail:    "failed",
	}
	skill.Workflow.Steps["write"] = WorkflowStep{
		Type:      "agent",
		Role:      "writer",
		Prompt:    pad,
		OnSuccess: "done",
		OnFail:    "failed",
	}
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(out) < swarmSkillSoftSizeLimit {
		t.Fatalf("test setup produced %d chars, want > %d", len(out), swarmSkillSoftSizeLimit)
	}
	report := ValidateSwarmSkillMarkdown(out, "big.md")
	if !hasCode(report.Findings, "file_size_soft") {
		t.Errorf("want file_size_soft warning, got %v", findingCodes(report.Findings))
	}
}

func TestSluggifyName(t *testing.T) {
	cases := map[string]string{
		"Hello World":     "hello-world",
		"Foo_Bar_Baz":     "foo-bar-baz",
		"-leading hyphen": "leading-hyphen",
		"trailing-":       "trailing",
		"  spaces  ":      "spaces",
		"already-good":    "already-good",
	}
	for in, want := range cases {
		if got := sluggifyName(in); got != want {
			t.Errorf("sluggifyName(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func findingCodes(findings []SwarmSkillFinding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = string(f.Severity) + ":" + f.Code
	}
	return out
}

func hasCode(findings []SwarmSkillFinding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// makeSkillBytesWithName produces a SWARM-SKILL.md file with the
// caller-chosen name field; everything else stays valid so the
// only finding we check for is the name rule under test.
func makeSkillBytesWithName(t *testing.T, name string) []byte {
	t.Helper()
	skill := makeTestSkill()
	skill.Name = name
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		// Marshal validates `name` is non-empty; for the
		// missing-name case we hand-build the bytes.
		return []byte(`---
description: d
version: 1.0.0
author: me
license: MIT
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: w
      entrypoint: s
      steps:
        s:
          type: agent
          role: r
          prompt: do
    roles:
      - name: r
        systemPrompt: hi
---
`)
	}
	return out
}

func makeSkillBytesWithVersion(t *testing.T, version string) []byte {
	t.Helper()
	skill := makeTestSkill()
	skill.Version = version
	skill.Workflow.Version = version
	out, err := MarshalSwarmSkill(skill, MarshalSwarmSkillOpts{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
