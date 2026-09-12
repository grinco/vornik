package registry

import (
	"testing"
)

// swarmWithRole builds a one-role swarm for the derivation tests.
func swarmWithRole(name string, tools ...string) *Swarm {
	return &Swarm{
		ID: "s",
		Roles: []SwarmRole{{
			Name:        name,
			Permissions: SwarmRolePermissions{AllowedTools: tools},
		}},
	}
}

func agentWorkflow(role string) *Workflow {
	return &Workflow{
		ID:         "wf",
		Entrypoint: "run",
		Steps: map[string]WorkflowStep{
			"run": {Type: "agent", Role: role, OnSuccess: "done"},
		},
	}
}

// TestWorkflowIsNetworkIncapable_AllInertToolsIsIncapable — the
// motivating case: the real companion `analyst` role's tool set, which
// is what companion-research-gather runs on (2026-09-11 incident).
func TestWorkflowIsNetworkIncapable_AllInertToolsIsIncapable(t *testing.T) {
	sw := swarmWithRole("analyst",
		"current_time", "file_read", "file_write",
		"read_many_files", "grep", "glob", "memory_search")
	if !WorkflowIsNetworkIncapable(agentWorkflow("analyst"), sw) {
		t.Fatal("a role holding only inert tools must derive as network-incapable")
	}
}

// TestWorkflowIsNetworkIncapable_NonInertToolMakesItCapable covers the
// three names §3 calls out by name plus — the case that matters most —
// a tool name the allow-list has NEVER SEEN. That last row pins the
// fail-safe direction: it would pass against the rejected deny-list
// form only by accident, and a suite that omitted it would not notice
// the polarity being flipped back.
func TestWorkflowIsNetworkIncapable_NonInertToolMakesItCapable(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"run_shell", "run_shell"},
		{"query_api", "query_api"},
		{"mcp tool", "mcp__scraper__web_fetch"},
		{"mcp tool, innocuous name", "mcp__forge__list_prs"},
		// The fail-safe row. No deny-list pattern matches this, and the
		// allow-list has never heard of it, so it MUST read as capable.
		{"never-seen tool name", "curl_exec"},
		{"never-seen tool name, harmless-looking", "spellcheck_run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := swarmWithRole("worker", "file_read", "grep", tc.tool)
			if WorkflowIsNetworkIncapable(agentWorkflow("worker"), sw) {
				t.Fatalf("role holding %q must NOT derive as network-incapable", tc.tool)
			}
		})
	}
}

// TestInertTools_ExcludesNetworkCapableTools is the maintenance guard on
// the list itself (review-20260912-2d8f). One careless addition
// silently converts the guard into the over-firing form two review
// rounds were spent removing, and nothing else in this design can catch
// that.
func TestInertTools_ExcludesNetworkCapableTools(t *testing.T) {
	for _, tool := range []string{
		"run_shell",
		"query_api",
		"mcp__scraper__web_fetch",
		"mcp__anything__at_all",
		"web_fetch",
	} {
		if ToolIsInert(tool) {
			t.Errorf("%q is on the inert allow-list — it can reach outside the daemon; "+
				"see the MAINTENANCE CONTRACT above inertTools", tool)
		}
	}
	// And the list is not accidentally empty, which would make every
	// assertion above pass while the guard never fired.
	if len(InertToolNames()) == 0 {
		t.Fatal("the inert allow-list is empty — the guard could never fire")
	}
	if !ToolIsInert("file_read") {
		t.Error("file_read must be inert — the list has lost its contents")
	}
}

// TestWorkflowIsNetworkIncapable_UnprovableCasesReturnFalse — every
// shape where the configuration does not PROVE incapability must fail
// safe (guard silent).
func TestWorkflowIsNetworkIncapable_UnprovableCasesReturnFalse(t *testing.T) {
	inert := swarmWithRole("analyst", "file_read")

	t.Run("nil workflow", func(t *testing.T) {
		if WorkflowIsNetworkIncapable(nil, inert) {
			t.Fatal("unknown workflow must not be claimed incapable")
		}
	})
	t.Run("nil swarm", func(t *testing.T) {
		if WorkflowIsNetworkIncapable(agentWorkflow("analyst"), nil) {
			t.Fatal("unresolvable swarm must not be claimed incapable")
		}
	})
	t.Run("role not in swarm", func(t *testing.T) {
		if WorkflowIsNetworkIncapable(agentWorkflow("ghost"), inert) {
			t.Fatal("a role the swarm does not define has unknown tools")
		}
	})
	t.Run("role declares no allowedTools", func(t *testing.T) {
		sw := &Swarm{ID: "s", Roles: []SwarmRole{{Name: "open"}}}
		if WorkflowIsNetworkIncapable(agentWorkflow("open"), sw) {
			t.Fatal("an empty allowedTools list means UNRESTRICTED in this codebase, not 'no tools'")
		}
	})
	t.Run("no agent-bearing step", func(t *testing.T) {
		wf := &Workflow{ID: "wf", Entrypoint: "g", Steps: map[string]WorkflowStep{
			"g": {Type: "gate"},
		}}
		if WorkflowIsNetworkIncapable(wf, inert) {
			t.Fatal("nothing was proven about a workflow with no agent step")
		}
	})
	for _, typ := range []string{"plan", "parallel", "system", "call_project", "spawn_project", "a2a_call"} {
		t.Run("step type "+typ, func(t *testing.T) {
			wf := &Workflow{ID: "wf", Entrypoint: "a", Steps: map[string]WorkflowStep{
				"a": {Type: "agent", Role: "analyst", OnSuccess: "b"},
				"b": {Type: typ, Role: "analyst"},
			}}
			if WorkflowIsNetworkIncapable(wf, inert) {
				t.Fatalf("a %q step can reach beyond its declared role; must not be claimed incapable", typ)
			}
		})
	}
}

// TestWorkflowIsNetworkIncapable_ResolvesRoleAliases — a step may name a
// role by an alias, the same substitution the executor's adaptive plan
// resolution performs. Failing to resolve it would read as "role not in
// swarm" and silently switch the guard off for that workflow.
func TestWorkflowIsNetworkIncapable_ResolvesRoleAliases(t *testing.T) {
	sw := &Swarm{ID: "s", Roles: []SwarmRole{{
		Name:        "rag-ingester",
		Aliases:     []string{"ingester"},
		Permissions: SwarmRolePermissions{AllowedTools: []string{"file_read", "glob"}},
	}}}
	if !WorkflowIsNetworkIncapable(agentWorkflow("ingester"), sw) {
		t.Fatal("an aliased role must resolve to the same tool set")
	}
}

// TestWorkflowIsNetworkIncapable_EveryRoleMustBeInert — one capable
// role anywhere in the workflow is enough.
func TestWorkflowIsNetworkIncapable_EveryRoleMustBeInert(t *testing.T) {
	sw := &Swarm{ID: "s", Roles: []SwarmRole{
		{Name: "analyst", Permissions: SwarmRolePermissions{AllowedTools: []string{"file_read"}}},
		{Name: "builder", Permissions: SwarmRolePermissions{AllowedTools: []string{"run_shell"}}},
	}}
	wf := &Workflow{ID: "wf", Entrypoint: "a", Steps: map[string]WorkflowStep{
		"a": {Type: "agent", Role: "analyst", OnSuccess: "b"},
		"b": {Type: "agent", Role: "builder", OnSuccess: "done"},
	}}
	if WorkflowIsNetworkIncapable(wf, sw) {
		t.Fatal("a workflow is incapable only when EVERY reachable role is")
	}
}

// TestShippedResearchGather_DerivesAsNetworkIncapable ties the
// derivation to the actual shipped configuration: the guard must fire
// on the exact workflow + swarm pair that produced the 2026-09-11
// incident, not merely on a fixture.
func TestShippedResearchGather_DerivesAsNetworkIncapable(t *testing.T) {
	reg := New()
	if err := reg.Load("../../configs"); err != nil {
		t.Fatalf("load shipped configs: %v", err)
	}
	wf := reg.GetWorkflow("companion-research-gather")
	if wf == nil {
		t.Fatal("companion-research-gather not in the shipped registry")
	}
	sw := reg.GetSwarm("companion-example-swarm")
	if sw == nil {
		t.Skip("companion swarm template not shipped in this edition")
	}
	if !WorkflowIsNetworkIncapable(wf, sw) {
		t.Fatal("companion-research-gather on the companion swarm must derive as network-incapable — " +
			"this is the exact pair that produced the 2026-09-11 incident")
	}
}
