package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validProjectBriefMarkdown covers every shape the parser
// handles: frontmatter with projectId + displayName, a `# Title`
// the parser strips, a preamble paragraph, all five known
// sections, and a non-canonical level-2 section that should land
// in Extra preserving order.
func validProjectBriefMarkdown() string {
	return `---
projectId: assistant-project
displayName: General-Purpose Assistant
---

# General-Purpose Assistant

A free-form intro that surfaces on the homepage hero. Operators
write whatever they want here; the parser captures it as the
description.

## Goal

Process incoming requests across research, planning, writing,
and recommendations.

## Audience

Operator-facing chat users who want quick answers and short
research.

## Success criteria

- Replies cite their sources.
- Multi-step requests get a plan before execution.

## Out of scope

- Code review or implementation tasks.
- Time-sensitive financial decisions.

## Risk & cadence

Low-risk conversational cadence. Autonomy off by default.

## References

Free-form notes about where the operator pulled the framing from.
`
}

// TestParseProjectMarkdown_HappyPath — every structural piece
// lands on the parsed brief: frontmatter fields, preamble (with
// the H1 title stripped), all five named sections, and the
// non-canonical `## References` section captured in Extra.
func TestParseProjectMarkdown_HappyPath(t *testing.T) {
	brief, err := ParseProjectMarkdown([]byte(validProjectBriefMarkdown()), "assistant.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v", err)
	}
	if brief.ProjectID != "assistant-project" {
		t.Errorf("ProjectID = %q, want assistant-project", brief.ProjectID)
	}
	if brief.DisplayName != "General-Purpose Assistant" {
		t.Errorf("DisplayName = %q, want General-Purpose Assistant", brief.DisplayName)
	}
	if !strings.Contains(brief.Description, "free-form intro") {
		t.Errorf("Description = %q, want preamble paragraph", brief.Description)
	}
	if strings.Contains(brief.Description, "# General-Purpose Assistant") {
		t.Errorf("Description still contains the H1 title: %q", brief.Description)
	}
	if !strings.Contains(brief.Goal, "research, planning") {
		t.Errorf("Goal = %q, want research/planning mention", brief.Goal)
	}
	if !strings.Contains(brief.Audience, "Operator-facing") {
		t.Errorf("Audience = %q, want operator-facing mention", brief.Audience)
	}
	if !strings.Contains(brief.SuccessCriteria, "cite their sources") {
		t.Errorf("SuccessCriteria = %q, want sources mention", brief.SuccessCriteria)
	}
	if !strings.Contains(brief.OutOfScope, "Code review") {
		t.Errorf("OutOfScope = %q, want code review mention", brief.OutOfScope)
	}
	if !strings.Contains(brief.RiskCadence, "Low-risk") {
		t.Errorf("RiskCadence = %q, want low-risk mention", brief.RiskCadence)
	}
	if len(brief.Extra) != 1 {
		t.Fatalf("Extra len = %d, want 1 unknown section captured", len(brief.Extra))
	}
	if brief.Extra[0].Heading != "References" {
		t.Errorf("Extra[0].Heading = %q, want References", brief.Extra[0].Heading)
	}
	if !strings.Contains(brief.Extra[0].Body, "where the operator pulled") {
		t.Errorf("Extra[0].Body = %q, want references body", brief.Extra[0].Body)
	}
}

// TestParseProjectMarkdown_MissingOpeningMarker — file that
// doesn't start with `---` is rejected with the shared message
// the SWARM.md/WORKFLOW.md parsers use, so operators see a
// consistent error.
func TestParseProjectMarkdown_MissingOpeningMarker(t *testing.T) {
	_, err := ParseProjectMarkdown([]byte("# no frontmatter\n"), "x.md")
	if err == nil || !strings.Contains(err.Error(), "opening frontmatter marker") {
		t.Errorf("err = %v, want missing-open message", err)
	}
}

// TestParseProjectMarkdown_MissingClosingMarker — opens but
// never closes the frontmatter.
func TestParseProjectMarkdown_MissingClosingMarker(t *testing.T) {
	md := `---
projectId: x
`
	_, err := ParseProjectMarkdown([]byte(md), "x.md")
	if err == nil || !strings.Contains(err.Error(), "closing frontmatter marker") {
		t.Errorf("err = %v, want missing-close message", err)
	}
}

// TestParseProjectMarkdown_MissingProjectID — frontmatter that
// parses but omits projectId fails loud. Without projectId the
// loader can't join the brief to its project.
func TestParseProjectMarkdown_MissingProjectID(t *testing.T) {
	md := `---
displayName: Floating Brief
---

## Goal

x

## Audience

x

## Success criteria

x
`
	_, err := ParseProjectMarkdown([]byte(md), "orphan.md")
	if err == nil || !strings.Contains(err.Error(), "projectId") {
		t.Errorf("err = %v, want missing-projectId message", err)
	}
}

// TestParseProjectMarkdown_MissingRequiredSection — each of the
// three required sections must be present and non-empty. We
// check Goal here; the loop in validateBriefRequired covers the
// other two by the same path.
func TestParseProjectMarkdown_MissingRequiredSection(t *testing.T) {
	md := `---
projectId: x
---

## Audience

ops users

## Success criteria

clear answers
`
	_, err := ParseProjectMarkdown([]byte(md), "no-goal.md")
	if err == nil || !strings.Contains(err.Error(), "## Goal") {
		t.Errorf("err = %v, want missing-Goal message", err)
	}
}

// TestParseProjectMarkdown_EmptyRequiredSection — a section
// heading with whitespace-only body counts as missing. Otherwise
// `## Goal\n\n## Audience` would silently slip an empty goal
// past the validator.
func TestParseProjectMarkdown_EmptyRequiredSection(t *testing.T) {
	md := `---
projectId: x
---

## Goal

## Audience

ops users

## Success criteria

clear answers
`
	_, err := ParseProjectMarkdown([]byte(md), "empty-goal.md")
	if err == nil || !strings.Contains(err.Error(), "## Goal") {
		t.Errorf("err = %v, want empty-Goal rejection", err)
	}
}

// TestParseProjectMarkdown_OptionalSectionsAbsent — a brief that
// only fills the three required sections is valid. OutOfScope
// and RiskCadence default to empty strings.
func TestParseProjectMarkdown_OptionalSectionsAbsent(t *testing.T) {
	md := `---
projectId: minimal
---

## Goal

g

## Audience

a

## Success criteria

s
`
	brief, err := ParseProjectMarkdown([]byte(md), "minimal.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v", err)
	}
	if brief.OutOfScope != "" {
		t.Errorf("OutOfScope = %q, want empty when section absent", brief.OutOfScope)
	}
	if brief.RiskCadence != "" {
		t.Errorf("RiskCadence = %q, want empty when section absent", brief.RiskCadence)
	}
	if len(brief.Extra) != 0 {
		t.Errorf("Extra len = %d, want 0", len(brief.Extra))
	}
}

// TestParseProjectMarkdown_DuplicateSection — two `## Goal`
// headings is operator error; fail loud so the second silently
// overwriting the first never happens.
func TestParseProjectMarkdown_DuplicateSection(t *testing.T) {
	md := `---
projectId: x
---

## Goal

first

## Audience

a

## Success criteria

s

## Goal

second
`
	_, err := ParseProjectMarkdown([]byte(md), "dup.md")
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want duplicate-section rejection", err)
	}
}

// TestParseProjectMarkdown_NoPreamble — body that opens directly
// with `## Goal` yields an empty Description, not an error. The
// preamble is optional.
func TestParseProjectMarkdown_NoPreamble(t *testing.T) {
	md := `---
projectId: x
---
## Goal

g

## Audience

a

## Success criteria

s
`
	brief, err := ParseProjectMarkdown([]byte(md), "no-preamble.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v", err)
	}
	if brief.Description != "" {
		t.Errorf("Description = %q, want empty for body that opens with H2", brief.Description)
	}
}

// TestParseProjectMarkdown_ExtraOrderPreserved — Extra sections
// are returned in source order so the editor round-trip writes
// them back the way the operator authored them.
func TestParseProjectMarkdown_ExtraOrderPreserved(t *testing.T) {
	md := `---
projectId: x
---

## Goal

g

## Audience

a

## Success criteria

s

## First extra

1st

## Second extra

2nd

## Third extra

3rd
`
	brief, err := ParseProjectMarkdown([]byte(md), "extras.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v", err)
	}
	if len(brief.Extra) != 3 {
		t.Fatalf("Extra len = %d, want 3", len(brief.Extra))
	}
	wantOrder := []string{"First extra", "Second extra", "Third extra"}
	for i, want := range wantOrder {
		if brief.Extra[i].Heading != want {
			t.Errorf("Extra[%d].Heading = %q, want %q", i, brief.Extra[i].Heading, want)
		}
	}
}

// TestParseProjectMarkdown_BOMTolerated — copy-paste from rich
// editors sometimes prepends a UTF-8 BOM. The shared
// splitFrontmatter helper strips it; this test pins that
// behaviour through the PROJECT.md entry point.
func TestParseProjectMarkdown_BOMTolerated(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte(validProjectBriefMarkdown())...)
	brief, err := ParseProjectMarkdown(content, "bom.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown with BOM: %v", err)
	}
	if brief.ProjectID != "assistant-project" {
		t.Errorf("ProjectID = %q after BOM strip, want assistant-project", brief.ProjectID)
	}
}

// TestParseProjectMarkdown_CRLF — Windows line endings must
// preserve the body sections after the frontmatter split.
func TestParseProjectMarkdown_CRLF(t *testing.T) {
	md := strings.ReplaceAll(validProjectBriefMarkdown(), "\n", "\r\n")
	brief, err := ParseProjectMarkdown([]byte(md), "crlf.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown CRLF: %v", err)
	}
	if !strings.Contains(brief.Goal, "research, planning") {
		t.Errorf("Goal = %q, want research/planning after CRLF split", brief.Goal)
	}
}

// TestLoadProjects_AttachesBrief — a project.yaml + matching
// projects/<id>.md together produce a Project whose Brief is
// populated and whose conflict-resolution rules are observed:
// brief displayName wins (YAML has none), brief preamble wins
// (YAML description empty), but project.yaml autonomy.goal stays
// authoritative because both are set.
func TestLoadProjects_AttachesBrief(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `projectId: "demo"
displayName: "YAML Display"
swarmId: "x-swarm"
defaultWorkflowId: "x-wf"
autonomy:
  enabled: false
  goal: "YAML goal stays authoritative"
`
	mdBody := `---
projectId: demo
displayName: Brief Display Wins
---

Preamble paragraph from the brief.

## Goal

Brief goal that should NOT override YAML goal.

## Audience

ops users

## Success criteria

clear answers
`
	if err := os.WriteFile(filepath.Join(pdir, "demo.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "demo.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	got, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}
	p, ok := got["demo"]
	if !ok {
		t.Fatal("project 'demo' missing from loader output")
	}
	if p.Brief == nil {
		t.Fatal("Brief = nil, want attached")
	}
	if p.DisplayName != "Brief Display Wins" {
		t.Errorf("DisplayName = %q, want brief frontmatter override", p.DisplayName)
	}
	if !strings.Contains(p.Description, "Preamble paragraph") {
		t.Errorf("Description = %q, want preamble (YAML had none)", p.Description)
	}
	if p.Autonomy.Goal != "YAML goal stays authoritative" {
		t.Errorf("Autonomy.Goal = %q, want YAML to keep precedence when set", p.Autonomy.Goal)
	}
}

// TestLoadProjects_BriefFillsAutonomyGoalWhenYAMLEmpty — the
// other half of the goal conflict-resolution rule: when YAML
// omits autonomy.goal, the brief's `## Goal` populates it so
// the autonomy loop has something to consume.
func TestLoadProjects_BriefFillsAutonomyGoalWhenYAMLEmpty(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `projectId: "demo"
swarmId: "x-swarm"
defaultWorkflowId: "x-wf"
autonomy:
  enabled: false
`
	mdBody := `---
projectId: demo
---

## Goal

Brief goal fills the empty YAML slot.

## Audience

ops users

## Success criteria

clear answers
`
	if err := os.WriteFile(filepath.Join(pdir, "demo.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "demo.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	got, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}
	if !strings.Contains(got["demo"].Autonomy.Goal, "Brief goal fills") {
		t.Errorf("Autonomy.Goal = %q, want brief to fill empty YAML slot", got["demo"].Autonomy.Goal)
	}
}

// TestLoadProjects_OrphanBriefRejected — a PROJECT.md whose
// projectId doesn't match any loaded project.yaml is a hard
// error. Catches typos at boot rather than letting the operator
// wonder why their brief never shows up.
func TestLoadProjects_OrphanBriefRejected(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mdBody := `---
projectId: missing
---

## Goal

g

## Audience

a

## Success criteria

s
`
	if err := os.WriteFile(filepath.Join(pdir, "orphan.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	_, err := LoadProjects(dir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want orphan-brief rejection", err)
	}
}

// TestLoadProjects_DuplicateBriefRejected — two PROJECT.md
// companions for the same projectId are ambiguous. Reject them
// instead of letting the later directory entry silently overwrite
// the first attached brief.
func TestLoadProjects_DuplicateBriefRejected(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `projectId: "demo"
swarmId: "x-swarm"
defaultWorkflowId: "x-wf"
`
	firstBrief := `---
projectId: demo
---

## Goal

first

## Audience

ops users

## Success criteria

clear answers
`
	secondBrief := `---
projectId: demo
---

## Goal

second

## Audience

ops users

## Success criteria

clear answers
`
	if err := os.WriteFile(filepath.Join(pdir, "demo.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "demo.md"), []byte(firstBrief), 0o644); err != nil {
		t.Fatalf("write first md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "demo-copy.md"), []byte(secondBrief), 0o644); err != nil {
		t.Fatalf("write second md: %v", err)
	}
	_, err := LoadProjects(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate PROJECT.md") {
		t.Errorf("err = %v, want duplicate-brief rejection", err)
	}
}

// TestLoadProjects_NoBriefStillLoads — projects without a
// PROJECT.md companion keep working unchanged. Phase 1A is
// additive; this pins that contract.
func TestLoadProjects_NoBriefStillLoads(t *testing.T) {
	dir := t.TempDir()
	pdir := filepath.Join(dir, "projects")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `projectId: "yaml-only"
displayName: "YAML Only"
swarmId: "x-swarm"
defaultWorkflowId: "x-wf"
`
	if err := os.WriteFile(filepath.Join(pdir, "yaml-only.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	got, err := LoadProjects(dir)
	if err != nil {
		t.Fatalf("LoadProjects: %v", err)
	}
	p, ok := got["yaml-only"]
	if !ok {
		t.Fatal("project missing")
	}
	if p.Brief != nil {
		t.Errorf("Brief = %+v, want nil for yaml-only project", p.Brief)
	}
}
