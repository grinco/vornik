package ui

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// buildSwarmsData powers the Swarms list page (IA completion): the global
// registry swarms, each with a role count, runtime summary, and the
// projects that use it. Pure builder so the row math is unit-tested
// without standing up a Server/templates.

func TestBuildSwarmsData_RowsSortedWithRuntimeAndUsage(t *testing.T) {
	swarms := []*registry.Swarm{
		{
			ID:          "dev-swarm",
			DisplayName: "Dev Swarm",
			LeadRole:    "lead",
			Roles: []registry.SwarmRole{
				{Name: "lead", RuntimePolicy: "ephemeral"},
				{Name: "coder"}, // empty policy → ephemeral default
			},
		},
		{
			ID: "assistant-swarm", // no DisplayName → label falls back to ID
			Roles: []registry.SwarmRole{
				{Name: "lead", RuntimePolicy: "warm"},
				{Name: "researcher", RuntimePolicy: "ephemeral"},
			},
		},
	}
	usage := projectUsage{bySwarm: map[string][]string{
		"dev-swarm":       {"autocoder"},
		"assistant-swarm": {"assistant", "janka"},
	}}

	data := buildSwarmsData(swarms, usage)

	if data.CurrentPage != "swarms" {
		t.Errorf("CurrentPage = %q; want swarms", data.CurrentPage)
	}
	if len(data.Rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(data.Rows))
	}
	// Sorted by label: "Dev Swarm" < "assistant-swarm"? Sort is
	// case-insensitive by label, so "assistant-swarm" < "Dev Swarm".
	if data.Rows[0].ID != "assistant-swarm" {
		t.Errorf("first row = %q; want assistant-swarm (case-insensitive label sort)", data.Rows[0].ID)
	}

	dev := data.Rows[1]
	if dev.Label != "Dev Swarm" || dev.RoleCount != 2 || dev.LeadRole != "lead" {
		t.Errorf("dev row = %+v; want label 'Dev Swarm', 2 roles, lead 'lead'", dev)
	}
	// All roles ephemeral (empty policy defaults to ephemeral) → "ephemeral".
	if dev.Runtime != "ephemeral" {
		t.Errorf("dev runtime = %q; want ephemeral", dev.Runtime)
	}
	if !reflect.DeepEqual(dev.UsedBy, []string{"autocoder"}) {
		t.Errorf("dev usedby = %v; want [autocoder]", dev.UsedBy)
	}

	asst := data.Rows[0]
	// Mixed warm + ephemeral → "mixed".
	if asst.Runtime != "mixed" {
		t.Errorf("assistant runtime = %q; want mixed", asst.Runtime)
	}
	if !reflect.DeepEqual(asst.UsedBy, []string{"assistant", "janka"}) {
		t.Errorf("assistant usedby = %v; want [assistant janka]", asst.UsedBy)
	}
}
