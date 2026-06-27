package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// validMarkdown returns a WORKFLOW.md body covering every shape
// the parser needs to handle: frontmatter with structural metadata,
// a Prompts section with multiple step subsections, and
// documentation sections the parser must ignore.
func validMarkdown() string {
	return `---
workflowId: "test-flow"
displayName: "Test Flow"
version: "1.0"
entrypoint: "plan"
maxStepVisits: 3
maxWallClock: "1h"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "implement"
    on_fail: "failed"
    timeout: "15m"
  implement:
    type: "agent"
    role: "coder"
    on_success: "review"
    on_fail: "failed"
  review:
    type: "agent"
    role: "reviewer"
    gates:
      - condition: "review.approved == true"
        target: "complete"
      - condition: "review.approved == false"
        target: "implement"
terminals:
  complete:
    status: "success"
  failed:
    status: "failed"
---

# Test Flow

A workflow used in unit tests.

## Prompts

### plan

Analyse the task and create an implementation plan. Break the work
into actionable steps before handing off.

### implement

Implement the feature according to the plan. Run tests before declaring
done.

### review

Inspect the implementation. Approve when it meets requirements.

## Error handling

Reviewer rejections route back to implement up to maxStepVisits=3.

## Sources

- Internal authoring guide
`
}

// TestParseWorkflowMarkdown_HappyPath — every structural piece
// lands on the parsed Workflow: ids, version, entrypoint, steps,
// terminals, gates, prompts from body.
func TestParseWorkflowMarkdown_HappyPath(t *testing.T) {
	wf, err := ParseWorkflowMarkdown([]byte(validMarkdown()), "test.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.ID != "test-flow" {
		t.Errorf("ID = %q, want test-flow", wf.ID)
	}
	if wf.DisplayName != "Test Flow" {
		t.Errorf("DisplayName = %q, want Test Flow", wf.DisplayName)
	}
	if wf.Entrypoint != "plan" {
		t.Errorf("Entrypoint = %q, want plan", wf.Entrypoint)
	}
	if wf.MaxStepVisits != 3 {
		t.Errorf("MaxStepVisits = %d, want 3", wf.MaxStepVisits)
	}
	if wf.MaxWallClock != "1h" {
		t.Errorf("MaxWallClock = %q, want 1h", wf.MaxWallClock)
	}
	if len(wf.Steps) != 3 {
		t.Fatalf("Steps len = %d, want 3", len(wf.Steps))
	}
	plan := wf.Steps["plan"]
	if !strings.Contains(plan.Prompt, "Analyse the task") {
		t.Errorf("plan.Prompt = %q, want body-derived prompt", plan.Prompt)
	}
	if plan.OnSuccess != "implement" {
		t.Errorf("plan.OnSuccess = %q, want implement", plan.OnSuccess)
	}
	review := wf.Steps["review"]
	if len(review.Gates) != 2 {
		t.Errorf("review.Gates len = %d, want 2", len(review.Gates))
	}
	if len(wf.Terminals) != 2 {
		t.Errorf("Terminals len = %d, want 2", len(wf.Terminals))
	}
}

// TestParseWorkflowMarkdown_FrontmatterPromptWins — when a step
// has both an inline frontmatter `prompt:` and a body subsection,
// the inline value is canonical (silent override). Documents and
// guards the precedence rule.
func TestParseWorkflowMarkdown_FrontmatterPromptWins(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    prompt: "inline wins"
---

## Prompts

### plan

body should be ignored
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "x.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.Steps["plan"].Prompt != "inline wins" {
		t.Errorf("plan.Prompt = %q, want 'inline wins'", wf.Steps["plan"].Prompt)
	}
}

// TestParseWorkflowMarkdown_MissingOpeningMarker — the parser
// hard-rejects files that don't begin with the `---` marker so a
// stray "everything looks fine but no frontmatter" file fails at
// load rather than producing a half-empty Workflow.
func TestParseWorkflowMarkdown_MissingOpeningMarker(t *testing.T) {
	md := `# Test
no frontmatter here
`
	_, err := ParseWorkflowMarkdown([]byte(md), "missing.md")
	if err == nil {
		t.Fatal("expected error on missing opening marker")
	}
	if !strings.Contains(err.Error(), "opening frontmatter marker") {
		t.Errorf("err = %v, want mention of opening frontmatter marker", err)
	}
}

// TestParseWorkflowMarkdown_OpeningMarkerNotOnOwnLine — defensive:
// `---something` on the first line shouldn't be treated as a valid
// frontmatter open.
func TestParseWorkflowMarkdown_OpeningMarkerNotOnOwnLine(t *testing.T) {
	md := `---workflowId: foo
`
	_, err := ParseWorkflowMarkdown([]byte(md), "bad-open.md")
	if err == nil || !strings.Contains(err.Error(), "own line") {
		t.Errorf("err = %v, want 'own line' rejection", err)
	}
}

// TestParseWorkflowMarkdown_MissingClosingMarker — opens with
// `---` but never closes. Hard error; the file is structurally
// malformed.
func TestParseWorkflowMarkdown_MissingClosingMarker(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"

# no closing marker
`
	_, err := ParseWorkflowMarkdown([]byte(md), "unclosed.md")
	if err == nil {
		t.Fatal("expected error on missing closing marker")
	}
	if !strings.Contains(err.Error(), "closing frontmatter marker") {
		t.Errorf("err = %v, want mention of closing frontmatter marker", err)
	}
}

// TestParseWorkflowMarkdown_FrontmatterParseError — YAML inside
// the frontmatter is malformed. Error message names the file.
func TestParseWorkflowMarkdown_FrontmatterParseError(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"
steps:
  plan:
    type: not-a-valid-mapping-without-a-key
   bad indent
---
`
	_, err := ParseWorkflowMarkdown([]byte(md), "yaml-bad.md")
	if err == nil {
		t.Fatal("expected YAML parse error")
	}
	if !strings.Contains(err.Error(), "yaml-bad.md") {
		t.Errorf("err = %v, want filename in message", err)
	}
}

// TestParseWorkflowMarkdown_UnknownStepInPromptsSection — a body
// subsection referencing an unknown step is a hard error so typos
// surface at load.
func TestParseWorkflowMarkdown_UnknownStepInPromptsSection(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    prompt: "p"
---

## Prompts

### typo-step

This step doesn't exist in the frontmatter.
`
	_, err := ParseWorkflowMarkdown([]byte(md), "typo.md")
	if err == nil || !strings.Contains(err.Error(), "no step 'typo-step'") {
		t.Errorf("err = %v, want unknown-step rejection", err)
	}
}

// TestParseWorkflowMarkdown_MissingPromptForAgentStep — the step
// has no inline prompt and no body subsection; load fails with a
// clear instruction on how to fix.
func TestParseWorkflowMarkdown_MissingPromptForAgentStep(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
---

## Prompts

(no subsections)
`
	_, err := ParseWorkflowMarkdown([]byte(md), "missing-prompt.md")
	if err == nil {
		t.Fatal("expected error on missing prompt")
	}
	if !strings.Contains(err.Error(), "no prompt") {
		t.Errorf("err = %v, want missing-prompt message", err)
	}
}

// TestParseWorkflowMarkdown_NonAgentStepsSkipPromptCheck — gate
// and approval steps don't require a prompt; verify the parser
// doesn't trip on them.
func TestParseWorkflowMarkdown_NonAgentStepsSkipPromptCheck(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "g"
steps:
  g:
    type: "gate"
    gates:
      - condition: "true"
        target: "done"
terminals:
  done:
    status: "success"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "gate.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.Steps["g"].Prompt != "" {
		t.Errorf("gate step picked up a prompt: %q", wf.Steps["g"].Prompt)
	}
}

// TestParseWorkflowMarkdown_LeadingBOMAndWhitespace — files
// downloaded through some editors or copy-pasted from a richtext
// surface carry a UTF-8 BOM. The parser must tolerate it.
func TestParseWorkflowMarkdown_LeadingBOMAndWhitespace(t *testing.T) {
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte("\n   ")...)
	content = append(content, []byte(validMarkdown())...)
	wf, err := ParseWorkflowMarkdown(content, "bom.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown with BOM + leading ws: %v", err)
	}
	if wf.ID != "test-flow" {
		t.Errorf("ID = %q, want test-flow", wf.ID)
	}
}

// TestParseWorkflowMarkdown_CRLF — Windows line endings must not
// corrupt the body offset after the closing frontmatter marker.
func TestParseWorkflowMarkdown_CRLF(t *testing.T) {
	md := strings.ReplaceAll(validMarkdown(), "\n", "\r\n")
	wf, err := ParseWorkflowMarkdown([]byte(md), "crlf.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown CRLF: %v", err)
	}
	if !strings.Contains(wf.Steps["plan"].Prompt, "Analyse the task") {
		t.Errorf("plan.Prompt = %q, want body-derived prompt after CRLF split", wf.Steps["plan"].Prompt)
	}
}

// TestParseWorkflowMarkdown_CleanupArtifactsField — opt-in canonical
// artifact pre-clean. The field is purely additive; legacy workflows
// without it must still parse cleanly (covered by the happy-path
// test). This one verifies the YAML→Go mapping.
func TestParseWorkflowMarkdown_CleanupArtifactsField(t *testing.T) {
	md := `---
workflowId: "research"
entrypoint: "research"
cleanup_artifacts:
  - artifacts/out/research.md
  - artifacts/out/summary.txt
steps:
  research:
    type: "agent"
    prompt: "do research"
terminals:
  done:
    status: "success"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "research.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if len(wf.CleanupArtifacts) != 2 {
		t.Fatalf("CleanupArtifacts len = %d, want 2", len(wf.CleanupArtifacts))
	}
	if wf.CleanupArtifacts[0] != "artifacts/out/research.md" {
		t.Errorf("CleanupArtifacts[0] = %q, want artifacts/out/research.md", wf.CleanupArtifacts[0])
	}
	if wf.CleanupArtifacts[1] != "artifacts/out/summary.txt" {
		t.Errorf("CleanupArtifacts[1] = %q, want artifacts/out/summary.txt", wf.CleanupArtifacts[1])
	}
}

// TestParseWorkflowMarkdown_RequireInputArtifactsField — opt-in
// front-matter flag that marks a workflow as artifact-only so the
// companion delegate handler rejects artifact-less delegations up
// front (2026-06-05 rag-ingest silent-skip incident). Purely
// additive; legacy workflows without it parse to the zero value
// (false), covered by the happy-path test. This pins the
// snake_case YAML→Go mapping the loader honors.
func TestParseWorkflowMarkdown_RequireInputArtifactsField(t *testing.T) {
	md := `---
workflowId: "ingest"
entrypoint: "ingest"
require_input_artifacts: true
steps:
  ingest:
    type: "agent"
    prompt: "ingest the staged files"
terminals:
  done:
    status: "success"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "ingest.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if !wf.RequireInputArtifacts {
		t.Fatalf("RequireInputArtifacts = false, want true")
	}

	// Absent field must default to false (legacy workflows).
	mdNoFlag := `---
workflowId: "plain"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
terminals:
  done:
    status: "success"
---
`
	wf2, err := ParseWorkflowMarkdown([]byte(mdNoFlag), "plain.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown (no flag): %v", err)
	}
	if wf2.RequireInputArtifacts {
		t.Errorf("RequireInputArtifacts = true for a workflow without the field, want false")
	}
}

// TestParseWorkflowMarkdown_BodySectionsBeyondPromptsIgnored —
// `## Error handling`, `## Sources`, and similar documentation
// sections are not consumed by the parser; their content doesn't
// leak into any step's prompt.
func TestParseWorkflowMarkdown_BodySectionsBeyondPromptsIgnored(t *testing.T) {
	wf, err := ParseWorkflowMarkdown([]byte(validMarkdown()), "v.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	for id, step := range wf.Steps {
		if strings.Contains(step.Prompt, "Reviewer rejections") {
			t.Errorf("step %q absorbed text from `## Error handling`: %q", id, step.Prompt)
		}
		if strings.Contains(step.Prompt, "Internal authoring guide") {
			t.Errorf("step %q absorbed text from `## Sources`: %q", id, step.Prompt)
		}
	}
}

// TestParseWorkflowMarkdown_HashEquivalentToYAML — the canonical
// Workflow.Hash() must be identical regardless of whether the
// workflow came from `.yaml` or `.md`. This is the executor's
// drift-detection contract.
func TestParseWorkflowMarkdown_HashEquivalentToYAML(t *testing.T) {
	yamlSrc := `workflowId: "h"
displayName: "Hash"
version: "1.0"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "do it"
    on_success: "done"
terminals:
  done:
    status: "success"
`
	mdSrc := `---
workflowId: "h"
displayName: "Hash"
version: "1.0"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "do it"
    on_success: "done"
terminals:
  done:
    status: "success"
---
`
	yamlWf := mustUnmarshalWorkflow(t, yamlSrc)
	mdWf, err := ParseWorkflowMarkdown([]byte(mdSrc), "h.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if yamlWf.Hash() != mdWf.Hash() {
		t.Errorf("hash drift: yaml=%s md=%s", yamlWf.Hash(), mdWf.Hash())
	}
}

// TestParseWorkflowMarkdown_PromptsSectionAbsent_InlineOnly — a
// workflow that puts every prompt inline doesn't need a Prompts
// section. The body may be empty or contain only docs sections.
func TestParseWorkflowMarkdown_PromptsSectionAbsent_InlineOnly(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    prompt: "do the thing"
    on_success: "done"
terminals:
  done:
    status: "success"
---

# Inline-only

## Notes

This workflow keeps prompts in the frontmatter.
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "inline.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.Steps["plan"].Prompt != "do the thing" {
		t.Errorf("plan.Prompt = %q, want 'do the thing'", wf.Steps["plan"].Prompt)
	}
}

// TestParseWorkflowMarkdown_NoStepsBlock — frontmatter without a
// `steps:` field returns a parsed Workflow with a nil Steps map
// (the YAML loader has the same behaviour). applyPrompts returns
// early; downstream Validate catches the structural issue.
func TestParseWorkflowMarkdown_NoStepsBlock(t *testing.T) {
	md := `---
workflowId: "x"
entrypoint: "go"
---
`
	wf, err := ParseWorkflowMarkdown([]byte(md), "no-steps.md")
	if err != nil {
		t.Fatalf("ParseWorkflowMarkdown: %v", err)
	}
	if wf.Steps != nil {
		t.Errorf("Steps = %v, want nil for frontmatter without steps block", wf.Steps)
	}
}

// TestParseWorkflowMarkdown_FrontmatterTooLongLine — a single
// frontmatter line that exceeds bufio.Scanner's buffer cap (4 MiB
// in this parser) surfaces as a "read frontmatter" error rather
// than silently truncating.
func TestParseWorkflowMarkdown_FrontmatterTooLongLine(t *testing.T) {
	// Build a frontmatter block whose ONE field's value is > 4 MiB
	// of contiguous text on a single line.
	huge := strings.Repeat("a", 5*1024*1024)
	md := "---\nworkflowId: \"x\"\ndescription: \"" + huge + "\"\n---\n"
	_, err := ParseWorkflowMarkdown([]byte(md), "huge-line.md")
	if err == nil || !strings.Contains(err.Error(), "read frontmatter") {
		t.Errorf("err = %v, want 'read frontmatter' failure", err)
	}
}

// TestParseWorkflowMarkdown_BodyTooLongLine — same defensive
// check on the body scanner.
func TestParseWorkflowMarkdown_BodyTooLongLine(t *testing.T) {
	huge := strings.Repeat("b", 5*1024*1024)
	md := "---\nworkflowId: \"x\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"p\"\n---\n\n## Prompts\n\n### plan\n" + huge + "\n"
	_, err := ParseWorkflowMarkdown([]byte(md), "huge-body.md")
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Errorf("err = %v, want 'read body' failure", err)
	}
}

// TestLoadWorkflows_IgnoresYAMLFiles — `.yaml` and `.yml` files
// in the workflows directory are silently skipped, same way a
// `.txt` would be. Stale YAML left over from the WORKFLOW.md
// migration is inert; operators can delete it at their own pace.
func TestLoadWorkflows_IgnoresYAMLFiles(t *testing.T) {
	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `workflowId: "legacy"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "x"
`
	if err := os.WriteFile(filepath.Join(workflowsDir, "legacy.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	got, err := LoadWorkflows(dir)
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadWorkflows over yaml-only dir = %d entries, want 0 (yaml is no longer a workflow format)", len(got))
	}
}

// TestLoadWorkflows_PrefersMDWhenBothExist — when an .md and a
// .yaml share the same workflowId, the loader picks the .md and
// ignores the .yaml. Resolves the operator-friendly "I still have
// the old yaml on disk during migration" case without a
// duplicate-ID error.
func TestLoadWorkflows_PrefersMDWhenBothExist(t *testing.T) {
	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlBody := `workflowId: "dup"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "stale yaml prompt"
`
	mdBody := `---
workflowId: "dup"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "canonical md prompt"
---
`
	if err := os.WriteFile(filepath.Join(workflowsDir, "dup.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "dup.md"), []byte(mdBody), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	got, err := LoadWorkflows(dir)
	if err != nil {
		t.Fatalf("LoadWorkflows over mixed dir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadWorkflows = %d entries, want 1 (yaml twin silently ignored)", len(got))
	}
	wf, ok := got["dup"]
	if !ok {
		t.Fatal("expected workflow 'dup' to be loaded from the .md")
	}
	if wf.Steps["go"].Prompt != "canonical md prompt" {
		t.Errorf("loaded prompt = %q, want the .md version", wf.Steps["go"].Prompt)
	}
}

// TestLoadWorkflows_MarkdownLoadError_Wrapped — a malformed
// WORKFLOW.md aborts LoadWorkflows; the error carries the
// filename.
func TestLoadWorkflows_MarkdownLoadError_Wrapped(t *testing.T) {
	dir := t.TempDir()
	workflowsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, "bad.md"), []byte("no frontmatter\n"), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	_, err := LoadWorkflows(dir)
	if err == nil || !strings.Contains(err.Error(), "bad.md") {
		t.Errorf("err = %v, want filename mention", err)
	}
}

// TestBundledWorkflowsParse — CI lint: every WORKFLOW.md file
// shipped under configs/workflows/ must parse cleanly and produce
// a Workflow whose ID matches the filename's stem. Catches future
// drift where someone edits a bundled template and breaks the
// parser without noticing.
func TestBundledWorkflowsParse(t *testing.T) {
	dir := "../../configs/workflows"
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
		wf, err := ParseWorkflowMarkdown(data, name)
		if err != nil {
			t.Errorf("%s parse: %v", name, err)
			continue
		}
		// ID should match the file stem (minus .md) so operators
		// can find the source from a workflowId reference.
		stem := strings.TrimSuffix(name, ".md")
		if wf.ID != stem {
			t.Errorf("%s: workflowId=%q does not match filename stem %q", name, wf.ID, stem)
		}
		// Every agent step must have a non-empty prompt — the
		// parser already enforces this, but assert defensively
		// in case a future loader change drops the check.
		for stepID, step := range wf.Steps {
			if step.Type == "agent" && strings.TrimSpace(step.Prompt) == "" {
				t.Errorf("%s: agent step %q has empty prompt", name, stepID)
			}
		}
	}
	if mdCount == 0 {
		t.Fatal("no WORKFLOW.md files found under configs/workflows — phase 3 migration regressed?")
	}
}

// mustUnmarshalWorkflow parses a YAML workflow source the same way
// LoadWorkflows does. Local helper so the hash-equivalence test
// doesn't depend on disk fixtures.
func mustUnmarshalWorkflow(t *testing.T, src string) *Workflow {
	t.Helper()
	var wf Workflow
	if err := yaml.Unmarshal([]byte(src), &wf); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	return &wf
}
