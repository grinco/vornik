package ui

import (
	"net/http"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// SwarmsData backs the Swarms list page (IA completion). Swarms are global
// registry entities, so every loaded swarm is shown (no per-project auth
// filtering — see ui-ia-completion-design.md §4.1).
type SwarmsData struct {
	Title       string
	CurrentPage string
	Rows        []SwarmRow
}

// SwarmRow is one row in the swarms table.
type SwarmRow struct {
	ID        string
	Label     string // DisplayName or ID
	RoleCount int
	LeadRole  string
	Runtime   string   // "ephemeral" | "warm" | "mixed" (summary across roles)
	UsedBy    []string // project labels referencing this swarm
}

// buildSwarmsData turns the registry's swarms + the project-usage index into
// the sorted view the template renders. Pure.
func buildSwarmsData(swarms []*registry.Swarm, usage projectUsage) SwarmsData {
	rows := make([]SwarmRow, 0, len(swarms))
	for _, sw := range swarms {
		if sw == nil {
			continue
		}
		label := sw.DisplayName
		if label == "" {
			label = sw.ID
		}
		rows = append(rows, SwarmRow{
			ID:        sw.ID,
			Label:     label,
			RoleCount: len(sw.Roles),
			LeadRole:  sw.LeadRole,
			Runtime:   summarizeRuntime(sw.Roles),
			UsedBy:    usage.bySwarm[sw.ID],
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Label) < strings.ToLower(rows[j].Label)
	})
	return SwarmsData{Title: "Swarms", CurrentPage: "swarms", Rows: rows}
}

// summarizeRuntime collapses the roles' RuntimePolicy into one label.
// Empty policy defaults to "ephemeral" (the daemon default). All-same →
// that value; otherwise "mixed". No roles → "ephemeral".
func summarizeRuntime(roles []registry.SwarmRole) string {
	seen := ""
	for _, r := range roles {
		p := r.RuntimePolicy
		if p == "" {
			p = "ephemeral"
		}
		if seen == "" {
			seen = p
		} else if seen != p {
			return "mixed"
		}
	}
	if seen == "" {
		return "ephemeral"
	}
	return seen
}

// SwarmsList renders the Swarms list page.
func (s *Server) SwarmsList(w http.ResponseWriter, r *http.Request) {
	var swarms []*registry.Swarm
	var usage projectUsage
	if s.projectReg != nil {
		swarms = s.projectReg.ListSwarms()
		usage = buildProjectUsage(s.projectReg.ListProjects())
	}
	s.render(w, "swarms.html", buildSwarmsData(swarms, usage))
}
