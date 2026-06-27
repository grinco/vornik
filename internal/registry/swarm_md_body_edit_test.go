package registry

import (
	"strings"
	"testing"
)

// swarmBodyFixture is a representative SWARM.md body shape:
// a level-1 title, a free-form paragraph, the `## Role prompts`
// section with two `### <role>` subsections, plus a trailing
// `## Notes` documentation section that must survive a surgical
// edit untouched.
const swarmBodyFixture = `# Test Swarm

A swarm used in unit tests.

## Role prompts

### lead

You are the lead. Plan the work, then delegate.

### coder

You are the coder. Implement one subtask at a time.
Write tests before production code.

## Notes

Other sections must survive role-prompt edits.
`

// TestReplaceSwarmRolePrompts_ReplacesSingleRole — updating one
// role's body leaves the other role's body intact AND keeps the
// `## Notes` section verbatim. This is the load-bearing case
// for the swarm editor: an operator iterating on the lead's
// prompt shouldn't disturb anything else.
func TestReplaceSwarmRolePrompts_ReplacesSingleRole(t *testing.T) {
	updated := map[string]string{
		"lead": "You are the lead. Plan only — never write code.",
	}
	out, err := ReplaceSwarmRolePrompts([]byte(swarmBodyFixture), updated)
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Plan only — never write code.") {
		t.Errorf("new lead prompt not written. got:\n%s", got)
	}
	if strings.Contains(got, "Plan the work, then delegate.") {
		t.Errorf("old lead prompt should have been replaced. got:\n%s", got)
	}
	if !strings.Contains(got, "Implement one subtask at a time.") {
		t.Errorf("coder prompt should have survived. got:\n%s", got)
	}
	if !strings.Contains(got, "## Notes") {
		t.Errorf("`## Notes` section should have survived. got:\n%s", got)
	}
	if !strings.Contains(got, "Other sections must survive") {
		t.Errorf("`## Notes` body content should have survived. got:\n%s", got)
	}
}

// TestReplaceSwarmRolePrompts_ReplacesMultipleRoles — covers the
// "save the whole form" case where every role's prompt is in the
// update map.
func TestReplaceSwarmRolePrompts_ReplacesMultipleRoles(t *testing.T) {
	updated := map[string]string{
		"lead":  "lead prompt v2",
		"coder": "coder prompt v2",
	}
	out, err := ReplaceSwarmRolePrompts([]byte(swarmBodyFixture), updated)
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "lead prompt v2") {
		t.Errorf("lead update missing. got:\n%s", got)
	}
	if !strings.Contains(got, "coder prompt v2") {
		t.Errorf("coder update missing. got:\n%s", got)
	}
}

// TestReplaceSwarmRolePrompts_NoChangeWhenRoleAbsent — a role
// in the update map that has no existing `### <role>` body
// section is APPENDED at the end of `## Role prompts`. This
// lets the swarm editor add a prompt for a role whose
// frontmatter exists but whose body was empty (rare, but the
// path matters for the create-role-then-fill-prompt flow).
func TestReplaceSwarmRolePrompts_AppendsMissingRoleBody(t *testing.T) {
	updated := map[string]string{
		"writer": "you are the writer; produce final output",
	}
	out, err := ReplaceSwarmRolePrompts([]byte(swarmBodyFixture), updated)
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "### writer") {
		t.Errorf("writer subsection not appended. got:\n%s", got)
	}
	if !strings.Contains(got, "you are the writer; produce final output") {
		t.Errorf("writer body not appended. got:\n%s", got)
	}
	// Existing roles untouched.
	if !strings.Contains(got, "Plan the work, then delegate.") {
		t.Errorf("lead body should be unchanged. got:\n%s", got)
	}
}

// TestReplaceSwarmRolePrompts_BodyWithoutRolePromptsSection —
// a swarm whose body has no `## Role prompts` section yet (all
// roles' prompts inlined in frontmatter) gains one when the
// edit map is non-empty.
func TestReplaceSwarmRolePrompts_AddsRolePromptsSection(t *testing.T) {
	body := `# Title

Free-form documentation.
`
	updated := map[string]string{
		"lead": "lead prompt",
	}
	out, err := ReplaceSwarmRolePrompts([]byte(body), updated)
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "## Role prompts") {
		t.Errorf("`## Role prompts` section not added. got:\n%s", got)
	}
	if !strings.Contains(got, "### lead") {
		t.Errorf("`### lead` subsection not added. got:\n%s", got)
	}
	if !strings.Contains(got, "# Title") {
		t.Errorf("original title should survive. got:\n%s", got)
	}
}

// TestReplaceSwarmRolePrompts_EmptyUpdates_NoOp — no role
// updates means no body changes at all. Defensive: we don't
// reformat the body or re-emit it canonically when there's
// nothing to do.
func TestReplaceSwarmRolePrompts_EmptyUpdates_NoOp(t *testing.T) {
	out, err := ReplaceSwarmRolePrompts([]byte(swarmBodyFixture), nil)
	if err != nil {
		t.Fatalf("ReplaceSwarmRolePrompts: %v", err)
	}
	if string(out) != swarmBodyFixture {
		t.Errorf("empty updates should produce verbatim output.\nwant:\n%s\ngot:\n%s", swarmBodyFixture, string(out))
	}
}

// TestSplitSwarmContent_Roundtrip — the public Split + Join
// pair is a clean inverse: split(content) → fm, body; then
// join(fm, body) reproduces the same bytes (modulo trailing
// newline normalisation).
func TestSplitSwarmContent_Roundtrip(t *testing.T) {
	content := []byte(`---
swarmId: t
leadRole: lead
roles:
  - name: lead
    systemPrompt: inline
    runtime:
      image: x
---

# Body

## Role prompts

### lead

lead body
`)
	fm, body, err := SplitSwarmContent(content, "t.md")
	if err != nil {
		t.Fatalf("SplitSwarmContent: %v", err)
	}
	if !strings.Contains(string(fm), "swarmId: t") {
		t.Errorf("frontmatter missing swarmId. got:\n%s", string(fm))
	}
	if !strings.Contains(string(body), "## Role prompts") {
		t.Errorf("body missing role prompts heading. got:\n%s", string(body))
	}

	joined := JoinSwarmContent(fm, body)
	// Re-parse to confirm the joined output is still a valid
	// SWARM.md — the canonical round-trip invariant.
	if _, err := ParseSwarmMarkdown(joined, "round.md"); err != nil {
		t.Errorf("re-parse failed after Split+Join: %v\nbody:\n%s", err, string(joined))
	}
}

// TestSortStrings_StableOrder — sortStrings is the deterministic
// helper that makes the append-roles ordering predictable. Pin
// the contract: result is a fresh sorted slice; input isn't
// mutated; duplicates stay in their sorted positions.
func TestSortStrings_StableOrder(t *testing.T) {
	in := []string{"c", "a", "b", "a"}
	got := sortStrings(in)
	want := []string{"a", "a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortStrings[%d] = %q want %q", i, got[i], want[i])
		}
	}
	// Input is untouched (defensive: callers may keep iterating
	// the original after a sort).
	if in[0] != "c" || in[3] != "a" {
		t.Errorf("input slice was mutated: %v", in)
	}
}

// TestSplitSwarmContent_RejectsNoFrontmatter — files without
// frontmatter are operator error; the Split helper surfaces the
// same message the parser would.
func TestSplitSwarmContent_RejectsNoFrontmatter(t *testing.T) {
	_, _, err := SplitSwarmContent([]byte("# just markdown\n"), "x.md")
	if err == nil || !strings.Contains(err.Error(), "opening frontmatter marker") {
		t.Errorf("err = %v, want missing-frontmatter rejection", err)
	}
}
