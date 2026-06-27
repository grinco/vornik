package ui

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// buildProjectUsage powers the "used by" column on the Swarms / Workflows
// list pages (IA completion): which projects reference each global swarm /
// workflow registry entity. Pure function over the project list.

func TestBuildProjectUsage_GroupsAndSorts(t *testing.T) {
	projects := []*registry.Project{
		{ID: "zeta", DisplayName: "Zeta", SwarmID: "dev-swarm", DefaultWorkflowID: "dev-pipeline"},
		{ID: "alpha", DisplayName: "Alpha", SwarmID: "dev-swarm", DefaultWorkflowID: "research"},
		{ID: "beta", SwarmID: "assistant-swarm", DefaultWorkflowID: "research"}, // no DisplayName → falls back to ID
	}
	u := buildProjectUsage(projects)

	// dev-swarm used by Alpha + Zeta (sorted by label); assistant-swarm by beta.
	if got := u.bySwarm["dev-swarm"]; !reflect.DeepEqual(got, []string{"Alpha", "Zeta"}) {
		t.Errorf("bySwarm[dev-swarm] = %v; want [Alpha Zeta]", got)
	}
	if got := u.bySwarm["assistant-swarm"]; !reflect.DeepEqual(got, []string{"beta"}) {
		t.Errorf("bySwarm[assistant-swarm] = %v; want [beta] (DisplayName falls back to ID)", got)
	}
	// research workflow used by Alpha + beta; dev-pipeline by Zeta.
	if got := u.byWorkflow["research"]; !reflect.DeepEqual(got, []string{"Alpha", "beta"}) {
		t.Errorf("byWorkflow[research] = %v; want [Alpha beta]", got)
	}
	if got := u.byWorkflow["dev-pipeline"]; !reflect.DeepEqual(got, []string{"Zeta"}) {
		t.Errorf("byWorkflow[dev-pipeline] = %v; want [Zeta]", got)
	}
}

func TestBuildProjectUsage_UnreferencedIsEmpty(t *testing.T) {
	u := buildProjectUsage([]*registry.Project{{ID: "p1", SwarmID: "s1", DefaultWorkflowID: "w1"}})
	if got := u.bySwarm["never-used"]; len(got) != 0 {
		t.Errorf("unreferenced swarm = %v; want empty", got)
	}
	if got := u.byWorkflow["never-used"]; len(got) != 0 {
		t.Errorf("unreferenced workflow = %v; want empty", got)
	}
}
