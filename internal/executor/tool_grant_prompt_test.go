package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The tool-budget block ships with the BINARY, not with swarm presets.
//
// An existing deployment's swarm YAML is the operator's file and an upgrade does not
// rewrite it. Guidance placed only in configs/swarms/*.md would therefore reach new
// installs and nobody else, leaving every current customer's agents unaware the tool
// exists. Injecting here means one upgrade updates every deployment.

func TestToolGrantPrompt_InjectedWhenAvailable(t *testing.T) {
	got := composeSystemPromptWithToolGrant("You are the researcher.", true)

	if !strings.Contains(got, "You are the researcher.") {
		t.Error("role identity lost; it must come FIRST — the block is housekeeping, not contract")
	}
	if !strings.Contains(got, "grant_step_tools") {
		t.Error("guidance does not name the tool, so an agent cannot call it")
	}
	if !strings.Contains(got, "escalation=true") {
		t.Error("no escalation hint: a lead that under-asks would treat a missing tool as a " +
			"dead end instead of asking for more")
	}
}

// TestToolGrantPrompt_AbsentWhenToolIsNot is the gate that matters. Telling an agent
// to call a tool this deployment does not serve wastes the tokens and invites a
// hallucinated call that fails.
func TestToolGrantPrompt_AbsentWhenToolIsNot(t *testing.T) {
	got := composeSystemPromptWithToolGrant("You are the researcher.", false)

	if strings.Contains(got, "grant_step_tools") {
		t.Error("advertised the grant tool on a deployment that does not serve it")
	}
	if got != "You are the researcher." {
		t.Errorf("prompt changed when the tool is unavailable: %q", got)
	}
}

// TestToolGrantPrompt_StaysCheap: this block is overhead on every prompt of every
// step, paid to save far more per iteration by shrinking the advertised tool surface.
// ~90 tokens is a trade worth making; 900 would not be, and a block nobody polices
// grows. (The measured saving lives in the registry LLD — a figure from one
// deployment's audit history does not belong in a shipped comment.)
func TestToolGrantPrompt_StaysCheap(t *testing.T) {
	const maxBytes = 800 // ~200 tokens
	if n := len(toolGrantSystemPromptBlock); n > maxBytes {
		t.Errorf("tool-budget block is %d bytes, over the %d-byte budget. It is added to "+
			"EVERY agent prompt, so a token-reduction feature must not become a token cost. "+
			"Trim it rather than raising this bound.", n, maxBytes)
	}
}

func TestToolGrantPrompt_EmptyRolePromptStillGetsBlock(t *testing.T) {
	if got := composeSystemPromptWithToolGrant("", true); !strings.Contains(got, "grant_step_tools") {
		t.Error("a role with no prompt of its own lost the guidance")
	}
}

// The block used to be gated on a GLOBAL "is the feature wired" flag, so it was
// injected into roles whose ceiling forbade the tool. Measured cost: nine 403s
// per review step, every run, on a swarm where no role could call it.
func TestRoleMayGrantTools(t *testing.T) {
	swarm := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "lead", Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read", "mcp__vornik__grant_step_tools"}}},
		{Name: "reviewer", Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read", "git_status"}}},
		{Name: "unrestricted", Permissions: registry.SwarmRolePermissions{}},
	}}

	cases := []struct {
		role string
		want bool
		why  string
	}{
		{"lead", true, "ceiling names the tool"},
		{"reviewer", false, "ceiling omits it — the block must not tell it to call one"},
		{"unrestricted", true, "an empty ceiling is unrestricted, as everywhere else"},
		{"absent", false, "a role not in the swarm cannot be permitted"},
		{"", false, "no role"},
	}
	for _, c := range cases {
		if got := RoleMayGrantTools(swarm, c.role); got != c.want {
			t.Errorf("RoleMayGrantTools(%q) = %v, want %v (%s)", c.role, got, c.want, c.why)
		}
	}
	if RoleMayGrantTools(nil, "lead") {
		t.Error("a nil swarm permitted the tool")
	}
}

// An operator may write either spelling in allowedTools.
func TestRoleMayGrantTools_AcceptsEitherSpelling(t *testing.T) {
	for _, spelling := range []string{"mcp__vornik__grant_step_tools", "grant_step_tools"} {
		swarm := &registry.Swarm{Roles: []registry.SwarmRole{
			{Name: "lead", Permissions: registry.SwarmRolePermissions{AllowedTools: []string{spelling}}},
		}}
		if !RoleMayGrantTools(swarm, "lead") {
			t.Errorf("spelling %q was not recognised", spelling)
		}
	}
}
