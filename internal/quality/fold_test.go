package quality

import "testing"

// The knob locus is (swarm, role), but the audit spine records (project, role).
// Shared swarms (assistant-swarm ⊇ {assistant, janka}) must have their
// per-project aggregates SUMMED before scoring — averaging pre-computed rates
// would misweight a busy project. This is the finding-#4 correctness core of
// the cost/quality tuning loop (design §C, review-20260721-a7bf).
func TestFoldRolesBySwarmSumsSharedProjects(t *testing.T) {
	in := []RoleAggregate{
		{ProjectID: "assistant", Role: "researcher", Total: 60, Passing: 50, PromptTokens: 600_000},
		{ProjectID: "janka", Role: "researcher", Total: 40, Passing: 30, PromptTokens: 400_000},
		{ProjectID: "assistant", Role: "writer", Total: 10, Passing: 9, PromptTokens: 20_000},
	}
	swarmOf := map[string]string{"assistant": "assistant-swarm", "janka": "assistant-swarm"}

	got := FoldRolesBySwarm(in, func(p string) string { return swarmOf[p] }, 20)

	researcher := findSwarmRole(got, "assistant-swarm", "researcher")
	if researcher == nil {
		t.Fatalf("no (assistant-swarm, researcher) aggregate in %+v", got)
	}
	if researcher.Total != 100 || researcher.Passing != 80 || researcher.PromptTokens != 1_000_000 {
		t.Errorf("researcher fold = {Total:%d Passing:%d Tokens:%d}, want {100 80 1000000}",
			researcher.Total, researcher.Passing, researcher.PromptTokens)
	}
	if researcher.MinSample != 20 {
		t.Errorf("MinSample = %d, want 20 (propagated for scoring)", researcher.MinSample)
	}
	// blast radius: both sharing projects recorded, sorted for stable display
	if len(researcher.Projects) != 2 || researcher.Projects[0] != "assistant" || researcher.Projects[1] != "janka" {
		t.Errorf("Projects = %v, want [assistant janka]", researcher.Projects)
	}

	writer := findSwarmRole(got, "assistant-swarm", "writer")
	if writer == nil || writer.Total != 10 || len(writer.Projects) != 1 {
		t.Errorf("writer fold wrong: %+v", writer)
	}
}

// A project with no resolvable swarm (deleted/renamed) is dropped, not folded
// under an empty swarm key — otherwise unrelated projects would pool together.
func TestFoldRolesBySwarmDropsUnresolvableProject(t *testing.T) {
	in := []RoleAggregate{{ProjectID: "ghost", Role: "researcher", Total: 30, Passing: 30, PromptTokens: 1}}
	got := FoldRolesBySwarm(in, func(string) string { return "" }, 20)
	if len(got) != 0 {
		t.Errorf("expected unresolvable project dropped, got %+v", got)
	}
}

func findSwarmRole(rs []SwarmRoleAggregate, swarm, role string) *SwarmRoleAggregate {
	for i := range rs {
		if rs[i].Swarm == swarm && rs[i].Role == role {
			return &rs[i]
		}
	}
	return nil
}
