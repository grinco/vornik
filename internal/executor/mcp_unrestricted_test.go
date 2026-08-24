package executor

import (
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The daemon's "role declares no allowedTools implies unrestricted MCP" rule was
// INVISIBLE in the input contract.
//
// buildAgentInput substitutes a four-tool default when a role declares none, so
// "declared nothing" and "declared exactly file_read/file_write/run_shell/
// current_time" arrive at the container identically. The container therefore
// cannot enforce MCP itself: two live roles are in the declared-nothing state
// (dispatcher in assistant-swarm and in easeit-companion-swarm), and a
// container-side gate would silently strip their MCP access.
//
// Saying what is meant is the first half of closing that gap
// (https://docs.vornik.io, MCP resolution-gap item; agent runtime contract §7.1). It is
// additive: an older agent image ignores the field.

func permissionsOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("task.json does not parse: %v", err)
	}
	cfg, ok := got["config"].(map[string]any)
	if !ok {
		t.Fatal("no config block")
	}
	perms, ok := cfg["permissions"].(map[string]any)
	if !ok {
		t.Fatal("no permissions block")
	}
	return perms
}

func TestBuildAgentInput_DeclaredNothingIsVisibleAsUnrestricted(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}

	// Permissions present, allowedTools empty — the "role declares none" case
	// the daemon reads as unrestricted.
	raw := buildAgentInput(task, "e1", "wf", "sw", "step", "dispatcher", "do it",
		&agentInputOpts{Permissions: &registry.SwarmRolePermissions{}})

	perms := permissionsOf(t, raw)
	if perms["mcpUnrestricted"] != true {
		t.Errorf("a role that declared no allowedTools must say so in the contract, got %v", perms["mcpUnrestricted"])
	}
}

func TestBuildAgentInput_DeclaredToolsAreNotUnrestricted(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}

	raw := buildAgentInput(task, "e1", "wf", "sw", "step", "coder", "do it",
		&agentInputOpts{Permissions: &registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read", "file_write", "run_shell", "current_time"},
		}})

	perms := permissionsOf(t, raw)
	if perms["mcpUnrestricted"] == true {
		t.Error("a role that declared tools is MCP-restricted, even when it declared exactly the default four")
	}
}

// The distinction the field exists for: these two payloads used to be
// indistinguishable, which is the whole reason the container cannot gate MCP.
func TestBuildAgentInput_TheTwoStatesAreNowDistinguishable(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	theDefaultFour := []string{"file_read", "file_write", "run_shell", "current_time"}

	declaredNothing := permissionsOf(t, buildAgentInput(task, "e1", "wf", "sw", "s", "r", "p",
		&agentInputOpts{Permissions: &registry.SwarmRolePermissions{}}))
	declaredTheFour := permissionsOf(t, buildAgentInput(task, "e1", "wf", "sw", "s", "r", "p",
		&agentInputOpts{Permissions: &registry.SwarmRolePermissions{AllowedTools: theDefaultFour}}))

	a, _ := json.Marshal(declaredNothing["allowedTools"])
	b, _ := json.Marshal(declaredTheFour["allowedTools"])
	if string(a) != string(b) {
		t.Fatalf("fixture drift: the two states are supposed to produce the same allowedTools (%s vs %s)", a, b)
	}
	if declaredNothing["mcpUnrestricted"] == declaredTheFour["mcpUnrestricted"] {
		t.Error("identical allowedTools and identical markers — the states are STILL indistinguishable")
	}
}

// No permissions at all is the same event as "declared nothing": the daemon
// substitutes the same default and applies no MCP narrowing.
func TestBuildAgentInput_NoPermissionsBlockIsUnrestricted(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}

	perms := permissionsOf(t, buildAgentInput(task, "e1", "wf", "sw", "s", "r", "p", nil))

	if perms["mcpUnrestricted"] != true {
		t.Errorf("no declared permissions means no MCP narrowing; the contract must say so, got %v", perms["mcpUnrestricted"])
	}
}
