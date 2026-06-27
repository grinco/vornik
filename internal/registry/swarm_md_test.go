package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validSwarmMarkdown returns a SWARM.md body covering every
// shape the parser handles: frontmatter with roles, a Role
// prompts section with multiple role subsections, and
// documentation sections the parser must ignore.
func validSwarmMarkdown() string {
	return `---
swarmId: "test-swarm"
displayName: "Test Swarm"
leadRole: "lead"
rolePrelude: "You are part of a test crew."
roles:
  - name: "lead"
    description: "Plans and delegates"
    runtime:
      image: "vornik/test:latest"
  - name: "coder"
    description: "Writes the implementation"
    runtime:
      image: "vornik/test:latest"
  - name: "reviewer"
    description: "Reviews the work"
    systemPrompt: "Inline reviewer prompt wins."
    runtime:
      image: "vornik/test:latest"
---

# Test Swarm

A swarm used in unit tests.

## Role prompts

### lead

You are the lead. Plan the work, then delegate. Hand subtasks to the
coder one at a time.

### coder

You are the coder. Implement one subtask at a time. Write tests
before production code.

### reviewer

This body subsection should be IGNORED because the frontmatter sets
systemPrompt explicitly.

## Notes

Other body sections are documentation.
`
}

// TestParseSwarmMarkdown_HappyPath — every structural piece lands
// on the parsed Swarm, plus prompts from body subsections fill
// the roles whose frontmatter omits systemPrompt.
func TestParseSwarmMarkdown_HappyPath(t *testing.T) {
	sw, err := ParseSwarmMarkdown([]byte(validSwarmMarkdown()), "test.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown: %v", err)
	}
	if sw.ID != "test-swarm" {
		t.Errorf("ID = %q, want test-swarm", sw.ID)
	}
	if sw.DisplayName != "Test Swarm" {
		t.Errorf("DisplayName = %q, want Test Swarm", sw.DisplayName)
	}
	if sw.LeadRole != "lead" {
		t.Errorf("LeadRole = %q, want lead", sw.LeadRole)
	}
	if !strings.Contains(sw.RolePrelude, "test crew") {
		t.Errorf("RolePrelude = %q, want test crew mention", sw.RolePrelude)
	}
	if len(sw.Roles) != 3 {
		t.Fatalf("Roles len = %d, want 3", len(sw.Roles))
	}
	byName := map[string]SwarmRole{}
	for _, r := range sw.Roles {
		byName[r.Name] = r
	}
	if got := byName["lead"].SystemPrompt; !strings.Contains(got, "Plan the work") {
		t.Errorf("lead.SystemPrompt = %q, want body-derived prompt", got)
	}
	if got := byName["coder"].SystemPrompt; !strings.Contains(got, "Implement one subtask") {
		t.Errorf("coder.SystemPrompt = %q, want body-derived prompt", got)
	}
	if got := byName["reviewer"].SystemPrompt; got != "Inline reviewer prompt wins." {
		t.Errorf("reviewer.SystemPrompt = %q, want frontmatter inline to win", got)
	}
}

// TestParseSwarmMarkdown_MissingOpeningMarker — same hard rejection
// as workflow_md to keep operator errors consistent.
func TestParseSwarmMarkdown_MissingOpeningMarker(t *testing.T) {
	_, err := ParseSwarmMarkdown([]byte("# no frontmatter\n"), "x.md")
	if err == nil || !strings.Contains(err.Error(), "opening frontmatter marker") {
		t.Errorf("err = %v, want missing-open message", err)
	}
}

// TestParseSwarmMarkdown_OpeningMarkerNotOnOwnLine — defensive.
func TestParseSwarmMarkdown_OpeningMarkerNotOnOwnLine(t *testing.T) {
	_, err := ParseSwarmMarkdown([]byte("---swarmId: foo\n"), "x.md")
	if err == nil || !strings.Contains(err.Error(), "own line") {
		t.Errorf("err = %v, want 'own line' rejection", err)
	}
}

// TestParseSwarmMarkdown_MissingClosingMarker — opens but never closes.
func TestParseSwarmMarkdown_MissingClosingMarker(t *testing.T) {
	md := `---
swarmId: "x"
leadRole: "lead"
roles:
  - name: "lead"
`
	_, err := ParseSwarmMarkdown([]byte(md), "x.md")
	if err == nil || !strings.Contains(err.Error(), "closing frontmatter marker") {
		t.Errorf("err = %v, want missing-close message", err)
	}
}

// TestParseSwarmMarkdown_FrontmatterParseError — malformed YAML
// in the frontmatter surfaces with the filename.
func TestParseSwarmMarkdown_FrontmatterParseError(t *testing.T) {
	md := `---
swarmId: "x"
roles:
  - name: "lead"
    description: ok
   bad indent
---
`
	_, err := ParseSwarmMarkdown([]byte(md), "bad-yaml.md")
	if err == nil {
		t.Fatal("expected yaml parse error")
	}
	if !strings.Contains(err.Error(), "bad-yaml.md") {
		t.Errorf("err = %v, want filename in message", err)
	}
}

// TestParseSwarmMarkdown_UnknownRoleInPromptsSection — body
// subsection referencing a role not in frontmatter fails loud.
func TestParseSwarmMarkdown_UnknownRoleInPromptsSection(t *testing.T) {
	md := `---
swarmId: "x"
leadRole: "lead"
roles:
  - name: "lead"
    description: ok
    systemPrompt: "inline"
---

## Role prompts

### typo-role

This role does not exist.
`
	_, err := ParseSwarmMarkdown([]byte(md), "typo.md")
	if err == nil || !strings.Contains(err.Error(), "no role 'typo-role'") {
		t.Errorf("err = %v, want unknown-role rejection", err)
	}
}

// TestParseSwarmMarkdown_RoleWithoutSystemPrompt_AllowsBuiltinFloor —
// a role declared without a systemPrompt and without a body
// subsection is LEGAL. The daemon's BuiltinRolePrelude +
// swarm.rolePrelude still apply at runtime; parser must not
// reject. Distinguishes from the workflow case where an agent
// step MUST have a prompt.
func TestParseSwarmMarkdown_RoleWithoutSystemPrompt_AllowsBuiltinFloor(t *testing.T) {
	md := `---
swarmId: "x"
leadRole: "lead"
roles:
  - name: "lead"
    description: ok
    runtime:
      image: "test"
---
`
	sw, err := ParseSwarmMarkdown([]byte(md), "bare.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown: %v", err)
	}
	if sw.Roles[0].SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want empty for bare role", sw.Roles[0].SystemPrompt)
	}
}

// TestParseSwarmMarkdown_BOMAndLeadingWhitespace — UTF-8 BOM
// tolerated.
func TestParseSwarmMarkdown_BOMAndLeadingWhitespace(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte("\n   ")...)
	content = append(content, []byte(validSwarmMarkdown())...)
	sw, err := ParseSwarmMarkdown(content, "bom.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown with BOM: %v", err)
	}
	if sw.ID != "test-swarm" {
		t.Errorf("ID = %q after BOM strip, want test-swarm", sw.ID)
	}
}

// TestParseSwarmMarkdown_CRLF — Windows line endings must preserve
// the markdown body after frontmatter splitting.
func TestParseSwarmMarkdown_CRLF(t *testing.T) {
	md := strings.ReplaceAll(validSwarmMarkdown(), "\n", "\r\n")
	sw, err := ParseSwarmMarkdown([]byte(md), "crlf.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown CRLF: %v", err)
	}
	byName := map[string]SwarmRole{}
	for _, r := range sw.Roles {
		byName[r.Name] = r
	}
	if !strings.Contains(byName["lead"].SystemPrompt, "Plan the work") {
		t.Errorf("lead.SystemPrompt = %q, want body-derived prompt after CRLF split", byName["lead"].SystemPrompt)
	}
}

// TestParseSwarmMarkdown_BodySectionsBeyondRolePromptsIgnored —
// `## Notes` and similar must NOT leak into any role's
// SystemPrompt.
func TestParseSwarmMarkdown_BodySectionsBeyondRolePromptsIgnored(t *testing.T) {
	sw, err := ParseSwarmMarkdown([]byte(validSwarmMarkdown()), "v.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown: %v", err)
	}
	for _, r := range sw.Roles {
		if strings.Contains(r.SystemPrompt, "Other body sections") {
			t.Errorf("role %q absorbed `## Notes` content: %q", r.Name, r.SystemPrompt)
		}
	}
}

// TestParseSwarmMarkdown_BodyTooLongLine — bufio.Scanner cap
// breach inside extractSections surfaces as a "read body" error
// rather than silent truncation.
func TestParseSwarmMarkdown_BodyTooLongLine(t *testing.T) {
	huge := strings.Repeat("b", 5*1024*1024)
	md := "---\nswarmId: \"x\"\nleadRole: \"lead\"\nroles:\n  - name: \"lead\"\n    description: ok\n    systemPrompt: \"x\"\n    runtime:\n      image: \"test\"\n---\n\n## Role prompts\n\n### lead\n" + huge + "\n"
	_, err := ParseSwarmMarkdown([]byte(md), "huge.md")
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Errorf("err = %v, want 'read body' failure", err)
	}
}

// TestParseSwarmMarkdown_EmptyBodySubsection_NoOp — a body
// subsection with no prose (e.g. just `### role-name` followed
// by another heading) doesn't overwrite the frontmatter
// systemPrompt. The applyRolePrompts loop skips empty body
// strings.
func TestParseSwarmMarkdown_EmptyBodySubsection_NoOp(t *testing.T) {
	md := `---
swarmId: "x"
leadRole: "lead"
roles:
  - name: "lead"
    description: "ok"
    systemPrompt: "frontmatter wins"
    runtime:
      image: "test"
---

## Role prompts

### lead

## Notes

prose after a docs heading
`
	sw, err := ParseSwarmMarkdown([]byte(md), "empty-body.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown: %v", err)
	}
	if sw.Roles[0].SystemPrompt != "frontmatter wins" {
		t.Errorf("SystemPrompt = %q, want frontmatter to be preserved", sw.Roles[0].SystemPrompt)
	}
}

// TestLoadSwarms_IgnoresYAMLFiles — `.yaml` and `.yml` files in
// the swarms directory are silently skipped. Same operator
// migration contract as workflows.
func TestLoadSwarms_IgnoresYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `swarmId: "legacy"
displayName: "Legacy"
leadRole: "lead"
roles:
  - name: "lead"
    description: "ok"
    systemPrompt: "x"
    runtime:
      image: "test"
`
	if err := os.WriteFile(filepath.Join(swarmsDir, "legacy.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	got, err := LoadSwarms(dir)
	if err != nil {
		t.Fatalf("LoadSwarms: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadSwarms over yaml-only dir = %d entries, want 0 (yaml is no longer a swarm format)", len(got))
	}
}

// TestLoadSwarms_PrefersMDWhenBothExist — operator-friendly
// migration contract: same swarmId in both formats → loader picks
// the .md and ignores the .yaml.
func TestLoadSwarms_PrefersMDWhenBothExist(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `swarmId: "dup"
displayName: "Stale YAML"
leadRole: "lead"
roles:
  - name: "lead"
    description: "ok"
    systemPrompt: "stale yaml prompt"
    runtime:
      image: "test"
`
	mdBody := `---
swarmId: "dup"
displayName: "Canonical MD"
leadRole: "lead"
roles:
  - name: "lead"
    description: "ok"
    systemPrompt: "canonical md prompt"
    runtime:
      image: "test"
---
`
	if err := os.WriteFile(filepath.Join(swarmsDir, "dup.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swarmsDir, "dup.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	got, err := LoadSwarms(dir)
	if err != nil {
		t.Fatalf("LoadSwarms over mixed dir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadSwarms = %d entries, want 1 (yaml twin silently ignored)", len(got))
	}
	sw, ok := got["dup"]
	if !ok {
		t.Fatal("expected swarm 'dup' to be loaded from the .md")
	}
	if sw.DisplayName != "Canonical MD" {
		t.Errorf("loaded DisplayName = %q, want the .md version", sw.DisplayName)
	}
	if sw.Roles[0].SystemPrompt != "canonical md prompt" {
		t.Errorf("loaded SystemPrompt = %q, want the .md version", sw.Roles[0].SystemPrompt)
	}
}

// TestBundledSwarmsParse — CI lint: every SWARM.md file shipped
// under configs/swarms/ must parse cleanly and produce a Swarm
// whose ID matches the filename's stem. Catches future drift the
// same way TestBundledWorkflowsParse does for workflows.
func TestBundledSwarmsParse(t *testing.T) {
	dir := "../../configs/swarms"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	mdCount := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		mdCount++
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sw, err := ParseSwarmMarkdown(data, name)
		if err != nil {
			t.Errorf("%s parse: %v", name, err)
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		if sw.ID != stem {
			t.Errorf("%s: swarmId=%q does not match filename stem %q", name, sw.ID, stem)
		}
		if len(sw.Roles) == 0 {
			t.Errorf("%s: zero roles parsed", name)
		}
	}
	if mdCount == 0 {
		t.Fatal("no SWARM.md files found under configs/swarms — phase-3-equivalent migration regressed?")
	}
}

// TestLoadSwarms_MarkdownLoadError_Wrapped — malformed .md aborts
// LoadSwarms with the filename in the error.
func TestLoadSwarms_MarkdownLoadError_Wrapped(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swarmsDir, "bad.md"), []byte("no frontmatter\n"), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	_, err := LoadSwarms(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("err = %v, want filename mention", err)
	}
}

// TestParseSwarmMarkdown_InBodyHeadingsDoNotDropPrompts — regression
// for the 2026-06-11 dev-swarm incident surfaced by `vornikctl doctor`
// (role_prompt_sanity flagged scout + architect as "systemPrompt is
// empty"). Root cause was in extractSections, not the config: a
// column-0 `## ` heading used *inside* a role prompt body flipped the
// section-walker out of the `## Role prompts` section, so every role
// after it parsed with an EMPTY systemPrompt — and an *indented*
// `### ` example line inside a body was mis-read as a real subsection.
//
// The fix: (1) recognise structural headings only at column 0, and
// (2) treat a column-0 `## ` heading as a section terminator only when
// no further `### ` subsection follows it (or it names a sibling
// section). This body reproduces both traps; pre-fix, "second" and
// "third" came back empty and the indented "### Example" tripped the
// unknown-role loud-fail.
func TestParseSwarmMarkdown_InBodyHeadingsDoNotDropPrompts(t *testing.T) {
	md := `---
swarmId: "hdr-swarm"
leadRole: "first"
roles:
  - name: "first"
    description: "one"
    runtime: {image: "x"}
  - name: "second"
    description: "two"
    runtime: {image: "x"}
  - name: "third"
    description: "three"
    runtime: {image: "x"}
---

## Role prompts

### first

Intro for first.

## Sub-heading inside a prompt body

More guidance for first. An indented example heading follows:

  ### Example: not a real subsection
  some template content

### second

Prompt for second.

### third

Prompt for third.

## Notes

Trailing documentation that must terminate the section.
`
	sw, err := ParseSwarmMarkdown([]byte(md), "hdr.md")
	if err != nil {
		t.Fatalf("ParseSwarmMarkdown: %v", err)
	}
	byName := map[string]SwarmRole{}
	for _, r := range sw.Roles {
		byName[r.Name] = r
	}
	if got := byName["first"].SystemPrompt; !strings.Contains(got, "Intro for first") ||
		!strings.Contains(got, "Sub-heading inside a prompt body") ||
		!strings.Contains(got, "### Example: not a real subsection") {
		t.Errorf("first lost in-body `## ` / indented `### ` content: %q", got)
	}
	if got := byName["second"].SystemPrompt; got != "Prompt for second." {
		t.Errorf("second.SystemPrompt = %q, want \"Prompt for second.\" (dropped by in-body heading)", got)
	}
	if got := byName["third"].SystemPrompt; got != "Prompt for third." {
		t.Errorf("third.SystemPrompt = %q, want \"Prompt for third.\"", got)
	}
	// The trailing `## Notes` must still terminate the section.
	for _, r := range sw.Roles {
		if strings.Contains(r.SystemPrompt, "Trailing documentation") {
			t.Errorf("role %q absorbed `## Notes`: %q", r.Name, r.SystemPrompt)
		}
	}
}
