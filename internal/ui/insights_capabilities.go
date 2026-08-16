package ui

// Capability adoption — which parts of the product a team has actually
// exercised, and which they have not touched.
//
// WHY BREADTH AND NOT VOLUME. The adoption page exists partly to create
// competition between proof-of-concept teams, and what a leaderboard rewards is
// what it produces. Ranking on event counts crowns whoever ran the most
// autonomy ticks and teaches nothing; ranking on how many DISTINCT capabilities
// a team has used makes the way to climb "try the next feature", which is what
// a proof of concept is for and what an enablement session can act on.
//
// The unused list is the other half and the more actionable one. A capability
// no team has touched is a feature the customer never discovered; a capability
// one team uses and six do not is a specific session with evidence behind it.

import (
	"context"
	"sort"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// capability is a catalogued product capability in operator-facing terms.
//
// Names and groups are curated rather than derived from the schema: a table is
// not a capability, and a customer-facing team needs "GitHub integration", not
// "webhook_events". Keys match the repository catalogue and are stable.
type capability struct {
	Key   string
	Label string
	Group string
	// Hint is what an enablement session would say about it. Present only where
	// a team plausibly SHOULD adopt it — an unused capability with no hint is
	// reported without being pitched.
	Hint string
}

var capabilityCatalogue = []capability{
	{Key: "tasks", Label: "Tasks & workflows", Group: "Core"},
	{Key: "tool_use", Label: "Agent tool use", Group: "Core"},
	{Key: "artifacts", Label: "Artifacts produced", Group: "Core"},

	{Key: "memory_search", Label: "Memory search (RAG)", Group: "Knowledge"},
	{Key: "memory_deposit", Label: "Memory deposits", Group: "Knowledge",
		Hint: "teams that only read memory get less from it than teams that also write to it"},
	{Key: "knowledge_graph", Label: "Knowledge graph", Group: "Knowledge"},
	{Key: "doc_extraction", Label: "Document extraction", Group: "Knowledge",
		Hint: "turns PDFs and specs into searchable project memory"},

	{Key: "chat", Label: "Chat channels", Group: "Channels"},
	{Key: "companion", Label: "Companion plugins (Claude, Codex)", Group: "Channels",
		Hint: "the lowest-friction entry point — usually the fastest adoption win"},
	{Key: "github", Label: "GitHub integration", Group: "Channels",
		Hint: "issues and PRs become tasks without anyone opening the UI"},
	{Key: "webhooks", Label: "Inbound webhooks", Group: "Channels"},

	{Key: "autonomy", Label: "Autonomy", Group: "Automation"},
	{Key: "scheduled", Label: "Scheduled tasks", Group: "Automation",
		Hint: "recurring work nobody has to remember to start"},
	{Key: "a2a_inbound", Label: "Agent-to-agent (inbound)", Group: "Automation"},
	{Key: "a2a_push", Label: "Agent-to-agent (push)", Group: "Automation"},
	{Key: "reminders", Label: "Reminders", Group: "Automation"},

	{Key: "instincts", Label: "Instincts", Group: "Advanced"},
	{Key: "control_plane", Label: "Control-plane proposals", Group: "Advanced"},
	{Key: "cross_project", Label: "Cross-project calls", Group: "Advanced",
		Hint: "lets one project delegate to another; unused everywhere so far"},
	{Key: "project_spawn", Label: "Project spawning", Group: "Advanced"},
	{Key: "web_write", Label: "Web write actions", Group: "Advanced"},
	{Key: "fixit", Label: "Fixit sessions", Group: "Advanced"},

	{Key: "quality_judging", Label: "Quality judging", Group: "Governance"},
	{Key: "budget_governance", Label: "Budget governance", Group: "Governance"},
	{Key: "gdpr_dsr", Label: "GDPR data-subject requests", Group: "Governance"},
}

// capabilityRow is one capability's adoption across the instance.
type capabilityRow struct {
	capability
	Events   int64
	Teams    int
	LastUsed *time.Time
	// Unused is true when no project exercised it in the window. These are the
	// enablement list and the reason the panel exists.
	Unused bool
}

// teamBreadth is one project's score on the competitive axis.
type teamBreadth struct {
	ProjectID string
	Used      int
	Total     int
	Pct       int
	// Missing names the capabilities this team has not touched that OTHERS
	// have. A capability nobody uses is an instance-wide gap, not this team's,
	// and listing it against them would be noise rather than a nudge.
	Missing []string
}

type capabilityStats struct {
	Rows   []capabilityRow
	Teams  []teamBreadth
	Total  int
	Unused int
}

// collectCapabilities builds the adoption picture. Nil repo yields zero rows,
// and the caller renders nothing rather than an empty table — "not wired" must
// not read as "nothing is used".
func (s *Server) collectCapabilities(ctx context.Context, since time.Time, projectScope []string) capabilityStats {
	var out capabilityStats
	if s.capabilityUsage == nil {
		return out
	}
	usage, err := s.capabilityUsage.Usage(ctx, since)
	if err != nil {
		return out
	}
	usage = filterCapabilityUsage(usage, projectScope)
	out.Rows, out.Total, out.Unused = capabilityRows(usage)
	out.Teams = teamBreadths(usage, out.Rows)
	return out
}

// filterCapabilityUsage applies the already-resolved UI scope to the
// repository's instance-wide result. A non-nil scope must never inherit rows
// from another project, nor instance-only signals whose owners cannot be
// established.
func filterCapabilityUsage(usage []persistence.CapabilityUsage, projectScope []string) []persistence.CapabilityUsage {
	if projectScope == nil {
		return usage
	}
	allowed := make(map[string]bool, len(projectScope))
	for _, id := range projectScope {
		if id != "" {
			allowed[id] = true
		}
	}
	out := make([]persistence.CapabilityUsage, 0, len(usage))
	for _, u := range usage {
		if allowed[u.ProjectID] {
			out = append(out, u)
		}
	}
	return out
}

// capabilityRows folds usage into one row per catalogued capability, most
// widely adopted first — a reader scanning down reaches the untouched features
// last, which is where the enablement list belongs.
func capabilityRows(usage []persistence.CapabilityUsage) (rows []capabilityRow, total, unused int) {
	type agg struct {
		events int64
		teams  map[string]bool
		last   *time.Time
	}
	byKey := map[string]*agg{}
	for _, u := range usage {
		a := byKey[u.Key]
		if a == nil {
			a = &agg{teams: map[string]bool{}}
			byKey[u.Key] = a
		}
		a.events += u.Count
		if u.Count > 0 && u.ProjectID != "" {
			a.teams[u.ProjectID] = true
		}
		if u.LastUsed != nil && (a.last == nil || u.LastUsed.After(*a.last)) {
			a.last = u.LastUsed
		}
	}
	total = len(capabilityCatalogue)
	for _, c := range capabilityCatalogue {
		row := capabilityRow{capability: c}
		if a := byKey[c.Key]; a != nil {
			row.Events, row.Teams, row.LastUsed = a.events, len(a.teams), a.last
		}
		row.Unused = row.Events == 0
		if row.Unused {
			unused++
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Teams != rows[j].Teams {
			return rows[i].Teams > rows[j].Teams
		}
		return rows[i].Events > rows[j].Events
	})
	return rows, total, unused
}

// teamBreadths scores each project on distinct capabilities used.
//
// Only capabilities SOMEBODY uses count. Scoring a team against features no one
// has discovered would make every team look equally bad and flatten exactly the
// difference the board exists to show.
func teamBreadths(usage []persistence.CapabilityUsage, rows []capabilityRow) []teamBreadth {
	adopted := make([]capability, 0, len(rows))
	for _, r := range rows {
		if !r.Unused {
			adopted = append(adopted, r.capability)
		}
	}
	perTeam := map[string]map[string]bool{}
	for _, u := range usage {
		if u.Count == 0 || u.ProjectID == "" {
			continue
		}
		if perTeam[u.ProjectID] == nil {
			perTeam[u.ProjectID] = map[string]bool{}
		}
		perTeam[u.ProjectID][u.Key] = true
	}
	var out []teamBreadth
	for pid, used := range perTeam {
		t := teamBreadth{ProjectID: pid, Total: len(adopted)}
		for _, c := range adopted {
			if used[c.Key] {
				t.Used++
			} else {
				t.Missing = append(t.Missing, c.Label)
			}
		}
		if t.Total > 0 {
			t.Pct = t.Used * 100 / t.Total
		}
		sort.Strings(t.Missing)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Used != out[j].Used {
			return out[i].Used > out[j].Used
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out
}
