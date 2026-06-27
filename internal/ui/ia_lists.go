package ui

import (
	"sort"

	"vornik.io/vornik/internal/registry"
)

// projectUsage is a reverse index from a global registry entity (swarm or
// workflow) to the projects that reference it — the data behind the "used
// by" column on the Swarms / Workflows IA-completion list pages. Project
// names are sorted for stable rendering.
type projectUsage struct {
	bySwarm    map[string][]string // swarmID            -> project labels
	byWorkflow map[string][]string // defaultWorkflowID  -> project labels
}

// buildProjectUsage builds the reverse index in one pass over the project
// list. O(projects); no persistence. A project with no DisplayName is
// labelled by its ID (projectLabel).
func buildProjectUsage(projects []*registry.Project) projectUsage {
	u := projectUsage{
		bySwarm:    make(map[string][]string),
		byWorkflow: make(map[string][]string),
	}
	for _, p := range projects {
		if p == nil {
			continue
		}
		label := projectLabel(p)
		if p.SwarmID != "" {
			u.bySwarm[p.SwarmID] = append(u.bySwarm[p.SwarmID], label)
		}
		if p.DefaultWorkflowID != "" {
			u.byWorkflow[p.DefaultWorkflowID] = append(u.byWorkflow[p.DefaultWorkflowID], label)
		}
	}
	for id := range u.bySwarm {
		sort.Strings(u.bySwarm[id])
	}
	for id := range u.byWorkflow {
		sort.Strings(u.byWorkflow[id])
	}
	return u
}

// projectLabel is the human-facing name for a project: its DisplayName when
// set, otherwise its ID.
func projectLabel(p *registry.Project) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.ID
}
