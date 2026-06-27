package registry

// Per-rule tests for the WORKFLOW.md phase-2 validator. Each rule
// gets its own minimal fixture so a regression on one finding
// can't ride on the back of another rule's failure.
//
// The cross-cutting `TestValidateWorkflowMarkdown_ShippedWorkflowsClean`
// (further down) is the "this is what production has to look
// like" gate: if a shipped workflow fails it, the validator is
// wrong, not the file. Treat that as a regression beacon for
// content-quality drift in this repo's own corpus.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalValid is the smallest frontmatter that satisfies every
// required rule. Tests mutate this template to introduce a
// single defect; the leading newline keeps the diff readable
// when a test prints the variant.
const minimalValid = `---
name: demo-skill
description: A tiny demo skill used by the validator's unit tests.
version: "1.0.0"
author: vornik
license: Apache-2.0
entrypoint: only
steps:
  only:
    type: agent
    role: lead
    on_success: done
terminals:
  done:
    status: COMPLETED
---

# Demo

## Prompts

### only

Do the thing.
`

// findingByCode is a tiny convenience: search the report for the
// first finding with the given code, returning a fresh zero
// value when absent so a missing-finding assertion can stay one
// line.
func findingByCode(report *WorkflowMDValidationReport, code string) (WorkflowMDFinding, bool) {
	for _, f := range report.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return WorkflowMDFinding{}, false
}

func TestValidateWorkflowMarkdown_AcceptsMinimal(t *testing.T) {
	report := ValidateWorkflowMarkdown([]byte(minimalValid), "demo.md")
	if report.HasErrors() {
		for _, f := range report.Findings {
			t.Logf("finding: %s", f)
		}
		t.Fatalf("expected no errors on minimal valid input; got %d findings", len(report.Findings))
	}
}

func TestValidateWorkflowMarkdown_MissingName(t *testing.T) {
	src := strings.Replace(minimalValid, "name: demo-skill\n", "", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "name_missing"); !ok {
		t.Fatalf("expected name_missing finding; got %v", report.Findings)
	}
	if !report.HasErrors() {
		t.Fatalf("expected ERROR severity for missing name")
	}
}

func TestValidateWorkflowMarkdown_WorkflowIDAliasAcceptsName(t *testing.T) {
	// Pre-phase-2 files spell the name `workflowId`. The
	// validator MUST accept that alias because the shipped
	// corpus uses it; the alias is the only reason the doctor
	// adapter can run cleanly over the existing repo.
	src := strings.Replace(minimalValid, "name: demo-skill\n", "workflowId: demo-skill\n", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "name_missing"); ok {
		t.Fatalf("workflowId alias should satisfy name_missing; got findings %v", report.Findings)
	}
	if report.HasErrors() {
		t.Fatalf("workflowId-aliased fixture should not error; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_NameShape(t *testing.T) {
	// Each entry: (name value, expect-finding).
	cases := []struct {
		name    string
		invalid bool
	}{
		{"demo-skill", false},
		{"demo-skill-2", false},
		{"demo", false},
		{"Demo-Skill", true},            // uppercase
		{"-demo", true},                 // leading hyphen
		{"demo-", true},                 // trailing hyphen
		{"demo--skill", true},           // consecutive hyphens
		{"demo_skill", true},            // underscore
		{"demo skill", true},            // whitespace
		{strings.Repeat("a", 65), true}, // too long
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(minimalValid, "name: demo-skill\n", "name: "+c.name+"\n", 1)
			report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
			_, hasShape := findingByCode(report, "name_shape")
			_, hasLong := findingByCode(report, "name_too_long")
			if c.invalid && !hasShape && !hasLong {
				t.Fatalf("expected a name finding for %q; got %v", c.name, report.Findings)
			}
			if !c.invalid && (hasShape || hasLong) {
				t.Fatalf("did not expect a name finding for %q; got %v", c.name, report.Findings)
			}
		})
	}
}

func TestValidateWorkflowMarkdown_DescriptionRequired(t *testing.T) {
	src := strings.Replace(minimalValid, "description: A tiny demo skill used by the validator's unit tests.\n", "", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "description_missing"); !ok {
		t.Fatalf("expected description_missing; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_DescriptionTooLong(t *testing.T) {
	long := strings.Repeat("a", workflowMDDescMaxLen+1)
	src := strings.Replace(minimalValid,
		"description: A tiny demo skill used by the validator's unit tests.\n",
		"description: "+long+"\n", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "description_too_long"); !ok {
		t.Fatalf("expected description_too_long; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_VersionRequired(t *testing.T) {
	src := strings.Replace(minimalValid, "version: \"1.0.0\"\n", "", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "version_missing"); !ok {
		t.Fatalf("expected version_missing; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_VersionShape(t *testing.T) {
	// Accept: 2-segment, 3-segment, pre-release, build meta.
	// Reject: Go-duration shape (the trading.md regression
	// motivated this check), bare words, leading-dot, trailing-dot.
	cases := []struct {
		version string
		ok      bool
	}{
		{"1.0", true},
		{"1.0.0", true},
		{"2.1.0", true},
		{"3.0", true},
		{"1.0.0-beta.1", true},
		{"1.0.0+build.42", true},
		{"25m", false},
		{"latest", false},
		{".1.0", false},
		{"1.", false},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			src := strings.Replace(minimalValid, "version: \"1.0.0\"\n", "version: \""+c.version+"\"\n", 1)
			report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
			_, has := findingByCode(report, "version_shape")
			if c.ok && has {
				t.Fatalf("version %q should be accepted; got %v", c.version, report.Findings)
			}
			if !c.ok && !has {
				t.Fatalf("version %q should be rejected; got %v", c.version, report.Findings)
			}
		})
	}
}

func TestValidateWorkflowMarkdown_AuthorAndLicenseRecommended(t *testing.T) {
	src := strings.Replace(minimalValid, "author: vornik\n", "", 1)
	src = strings.Replace(src, "license: Apache-2.0\n", "", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "author_missing"); !ok {
		t.Fatalf("expected author_missing warning; got %v", report.Findings)
	}
	if _, ok := findingByCode(report, "license_missing"); !ok {
		t.Fatalf("expected license_missing warning; got %v", report.Findings)
	}
	if report.HasErrors() {
		t.Fatalf("missing author/license should be WARNING only; got %v", report.Findings)
	}
	if !report.HasWarnings() {
		t.Fatalf("expected at least one warning")
	}
}

func TestValidateWorkflowMarkdown_RelatedSkillsList(t *testing.T) {
	withRelated := strings.Replace(minimalValid, "license: Apache-2.0\n",
		"license: Apache-2.0\nmetadata:\n  related_skills:\n    - sibling-skill\n    - another-skill\n", 1)
	report := ValidateWorkflowMarkdown([]byte(withRelated), "demo.md")
	if report.HasErrors() {
		t.Fatalf("valid related_skills list should not error; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_RelatedSkillsBadEntry(t *testing.T) {
	withRelated := strings.Replace(minimalValid, "license: Apache-2.0\n",
		"license: Apache-2.0\nmetadata:\n  related_skills:\n    - sibling-skill\n    - Bad_Entry\n    - \"\"\n", 1)
	report := ValidateWorkflowMarkdown([]byte(withRelated), "demo.md")
	if _, ok := findingByCode(report, "related_skills_shape_entry"); !ok {
		t.Fatalf("expected related_skills_shape_entry for Bad_Entry; got %v", report.Findings)
	}
	if _, ok := findingByCode(report, "related_skills_empty"); !ok {
		t.Fatalf("expected related_skills_empty for blank entry; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_RelatedSkillsWrongShape(t *testing.T) {
	withRelated := strings.Replace(minimalValid, "license: Apache-2.0\n",
		"license: Apache-2.0\nmetadata:\n  related_skills: not-a-list\n", 1)
	report := ValidateWorkflowMarkdown([]byte(withRelated), "demo.md")
	if _, ok := findingByCode(report, "related_skills_shape"); !ok {
		t.Fatalf("expected related_skills_shape; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_FileSizeHardCap(t *testing.T) {
	// Build a file that exceeds the hard cap by adding a giant
	// trailing prose block. The frontmatter stays valid so this
	// is exactly the "single skill grew too big" case.
	bloat := strings.Repeat("a", workflowMDHardSizeLimit+10)
	src := minimalValid + "\n" + bloat
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "file_size_hard"); !ok {
		t.Fatalf("expected file_size_hard; got %v", report.Findings)
	}
	if !report.HasErrors() {
		t.Fatalf("oversized file should fail with ERROR")
	}
}

func TestValidateWorkflowMarkdown_FileSizeSoftWarning(t *testing.T) {
	// Between soft (15k) and hard (100k) — warns but does not
	// fail.
	bloat := strings.Repeat("a", workflowMDSoftSizeLimit+10)
	src := minimalValid + "\n" + bloat
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "file_size_soft"); !ok {
		t.Fatalf("expected file_size_soft; got %v", report.Findings)
	}
	if report.HasErrors() {
		t.Fatalf("soft-cap-only file should not error; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_FrontmatterMissing(t *testing.T) {
	// A body-only file lacking the opening marker.
	src := "# Just a doc\n\nHello.\n"
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "frontmatter_split"); !ok {
		t.Fatalf("expected frontmatter_split; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_FrontmatterEmpty(t *testing.T) {
	src := "---\n---\n\nbody\n"
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "frontmatter_empty"); !ok {
		t.Fatalf("expected frontmatter_empty; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_FrontmatterNotYAML(t *testing.T) {
	src := "---\nname: [unclosed\n---\n\nbody\n"
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "frontmatter_yaml"); !ok {
		t.Fatalf("expected frontmatter_yaml; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_PromptsSectionMissing(t *testing.T) {
	// Strip the `## Prompts` block entirely. The agent step's
	// role is set and no inline prompt — the validator must
	// emit BOTH the section-missing finding and a per-step one.
	src := strings.Split(minimalValid, "## Prompts")[0]
	src = strings.TrimRight(src, "\n") + "\n"
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "prompts_section_missing"); !ok {
		t.Fatalf("expected prompts_section_missing; got %v", report.Findings)
	}
	if _, ok := findingByCode(report, "prompt_step_missing"); !ok {
		t.Fatalf("expected prompt_step_missing; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_PromptStepMissing(t *testing.T) {
	// `## Prompts` is present but the subsection for the agent
	// step isn't. Only the per-step finding fires.
	src := strings.Replace(minimalValid, "### only\n\nDo the thing.\n", "### other\n\nUnrelated.\n", 1)
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "prompts_section_missing"); ok {
		t.Fatalf("did not expect prompts_section_missing; got %v", report.Findings)
	}
	if _, ok := findingByCode(report, "prompt_step_missing"); !ok {
		t.Fatalf("expected prompt_step_missing; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_InlinePromptSatisfies(t *testing.T) {
	// Agent step with an inline `prompt:` field; body has no
	// `## Prompts` section. No finding should fire.
	src := `---
name: inline-prompt-demo
description: Validator test fixture; inline prompt; no body subsection.
version: "1.0.0"
author: vornik
license: Apache-2.0
entrypoint: only
steps:
  only:
    type: agent
    role: lead
    prompt: "do the thing"
    on_success: done
terminals:
  done:
    status: COMPLETED
---

# Body without a Prompts section.
`
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if report.HasErrors() {
		t.Fatalf("inline prompt should satisfy the symmetry rule; got %v", report.Findings)
	}
}

func TestValidateWorkflowMarkdown_GateStepNoPromptNeeded(t *testing.T) {
	// Gate steps don't carry prompts; the rule must not fire.
	src := `---
name: gate-only
description: Validator test fixture for a gate-only workflow with no agent steps.
version: "1.0.0"
author: vornik
license: Apache-2.0
entrypoint: g
steps:
  g:
    type: gate
    on_success: done
terminals:
  done:
    status: COMPLETED
---

# Body with no Prompts section.
`
	report := ValidateWorkflowMarkdown([]byte(src), "demo.md")
	if _, ok := findingByCode(report, "prompt_step_missing"); ok {
		t.Fatalf("gate step should not require a body prompt; got %v", report.Findings)
	}
}

// TestValidateWorkflowMarkdown_ShippedWorkflowsClean is the
// "the corpus must pass" gate. Failure here is a validator bug
// (or, in rare cases, a real content-quality regression a
// reviewer needs to look at — but treat that case as the
// exception, not the rule).
func TestValidateWorkflowMarkdown_ShippedWorkflowsClean(t *testing.T) {
	root := repoRootFromRegistryTest(t)
	dir := filepath.Join(root, "configs", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read configs/workflows: %v", err)
	}
	required := map[string]bool{
		"adaptive.md":        false,
		"dev-pipeline.md":    false,
		"plan-and-write.md":  false,
		"research.md":        false,
		"simple-workflow.md": false,
		"trading.md":         false,
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		report := ValidateWorkflowMarkdown(data, e.Name())
		if report.HasErrors() {
			for _, f := range report.Findings {
				if f.Severity == SeverityError {
					t.Errorf("%s: %s", e.Name(), f)
				}
			}
		}
		if _, ok := required[e.Name()]; ok {
			required[e.Name()] = true
		}
	}
	for name, seen := range required {
		if !seen {
			t.Errorf("required workflow %s not found in configs/workflows/", name)
		}
	}
}

// TestSuggestNameShape covers the `--fix` hint logic. The function
// is small and pure; this keeps it that way.
func TestSuggestNameShape(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"Demo-Skill", "demo-skill"},
		{"demo_skill", "demo-skill"},
		{"demo skill", "demo-skill"},
		{"--demo--", "demo"},
		{"DEMO 123!!!", "demo-123"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := suggestNameShape(c.in)
			if got != c.want {
				t.Fatalf("suggestNameShape(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

// TestStringSort_NeedsSwap exercises the inner-swap branch of
// the tiny insertion sort. Without an out-of-order input the
// branch never fires and coverage stays partial.
func TestStringSort_NeedsSwap(t *testing.T) {
	s := []string{"c", "a", "b"}
	stringSort(s)
	if s[0] != "a" || s[1] != "b" || s[2] != "c" {
		t.Fatalf("stringSort wrong: %v", s)
	}
	// Already-sorted input takes the early-exit path.
	s2 := []string{"a", "b", "c"}
	stringSort(s2)
	if s2[0] != "a" || s2[2] != "c" {
		t.Fatalf("stable on sorted input broke: %v", s2)
	}
}

// TestWorkflowMDValidationReport_HasWarningsAndErrors covers
// the predicate methods directly — the integration tests above
// only exercise the True branches of each.
func TestWorkflowMDValidationReport_HasWarningsAndErrors(t *testing.T) {
	empty := &WorkflowMDValidationReport{}
	if empty.HasErrors() {
		t.Fatalf("empty report should not HasErrors")
	}
	if empty.HasWarnings() {
		t.Fatalf("empty report should not HasWarnings")
	}
	onlyWarn := &WorkflowMDValidationReport{Findings: []WorkflowMDFinding{
		{Severity: SeverityWarning, Code: "x"},
	}}
	if onlyWarn.HasErrors() {
		t.Fatalf("warning-only should not HasErrors")
	}
	if !onlyWarn.HasWarnings() {
		t.Fatalf("warning-only should HasWarnings")
	}
}

// TestWorkflowMDFinding_String pins the single-line CLI format
// because the doctor adapter's Items field re-uses it. Format
// change would silently churn doctor output.
func TestWorkflowMDFinding_String(t *testing.T) {
	f := WorkflowMDFinding{
		Severity: SeverityError,
		Code:     "name_missing",
		Field:    "name",
		Message:  "`name` is required",
	}
	want := "[ERROR] name_missing: name — `name` is required"
	if got := f.String(); got != want {
		t.Fatalf("String() = %q; want %q", got, want)
	}
	// Without a field, the format must collapse cleanly so the
	// CLI doesn't print a dangling colon.
	f2 := WorkflowMDFinding{Severity: SeverityWarning, Code: "file_size_soft", Message: "big"}
	want2 := "[WARNING] file_size_soft — big"
	if got := f2.String(); got != want2 {
		t.Fatalf("String() w/o field = %q; want %q", got, want2)
	}
}

// repoRootFromRegistryTest is the registry-package equivalent
// of the executor-package helper. Pulled in here rather than
// exported across packages to keep the validator's test file
// self-contained.
func repoRootFromRegistryTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod walking up from %s", dir)
	return ""
}
