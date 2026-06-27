package ui

// navDest is one destination inside a nav area's contextual panel.
// Key matches the page's CurrentPage token so the panel can mark the
// active entry with the same value handlers already set today.
type navDest struct {
	Key   string
	Label string
	Href  string
	Icon  string // template name of the inline SVG, e.g. "navIconSwarms"
	// AdminOnly hides this destination from non-admins, mirroring the
	// area-level flag. Set for daemon-global / cross-project surfaces a
	// project-scoped RoleUser session is denied server-side
	// (sessionUserGlobalAuthoringPrefixes) — without it those users saw
	// nav links that 403 on click.
	AdminOnly bool
	// Cap names an optional capability this destination requires (e.g.
	// "trading"). Rendered as data-cap; the nav JS hides it when the
	// login caps cookie is present and omits the capability (fail-open:
	// no cookie → shown). Used for surfaces that are harmless-but-
	// irrelevant rather than access-denied (Trading degrades gracefully).
	Cap string
}

// navAreaDef is one top-level area shown in the icon rail. AdminOnly
// areas are rendered hidden-by-default and unhidden by the same
// IsAdmin / marker-cookie path used for the legacy Admin link.
type navAreaDef struct {
	Key   string
	Label string
	Icon  string
	// Href is the rail icon's click target — the area's primary
	// destination. Kept as an explicit field (not index .Dests 0) because
	// the template FuncMap overrides the builtin `index`.
	Href      string
	AdminOnly bool
	Dests     []navDest
}

// navModel is the single source of truth for the navigation IA. The
// icon rail, the contextual panel, and the mobile drawer all render
// from this slice so they cannot drift apart.
func navModel() []navAreaDef {
	return []navAreaDef{
		// Steer leads the rail: the operator's live-control surface — watch
		// what's running (Live) and act on what's blocked (Needs you). Named
		// for the existing "steering" affordance (inline hint injection on the
		// live page). Distinct from Orchestration (authoring/managing the
		// catalog) so the do-something-now surfaces aren't buried among the
		// list pages.
		{Key: "steer", Label: "Steer", Icon: "navIconSteer", Href: "/ui/live", Dests: []navDest{
			{Key: "live", Label: "Live", Href: "/ui/live", Icon: "navIconLive"},
			{Key: "inbox", Label: "Needs you", Href: "/ui/inbox", Icon: "navIconInbox"},
		}},
		// Tasks leads the Orchestration area: it's where the operator most
		// often works, so it's the default destination (the rail icon lands
		// here). Projects (the container for swarms/workflows/tasks), Swarms,
		// Workflows, and Executions follow. Each dest points at its
		// first-class top-level list page (IA completion, 2026-06-09):
		// Swarms/Workflows list the global registry entities; Executions is
		// the cross-task run list. Row click-through reaches the existing
		// detail/edit surfaces.
		{Key: "orchestration", Label: "Orchestration", Icon: "navIconOrchestration", Href: "/ui/tasks", Dests: []navDest{
			{Key: "tasks", Label: "Tasks", Href: "/ui/tasks", Icon: "navIconTasks"},
			{Key: "projects", Label: "Projects", Href: "/ui/projects", Icon: "navIconProjects"},
			{Key: "swarms", Label: "Swarms", Href: "/ui/swarms", Icon: "navIconSwarms", AdminOnly: true},
			{Key: "workflows", Label: "Workflows", Href: "/ui/workflows", Icon: "navIconWorkflows", AdminOnly: true},
			{Key: "executions", Label: "Executions", Href: "/ui/executions", Icon: "navIconExecutions"},
		}},
		{Key: "memory", Label: "Memory", Icon: "navIconMemory", Href: "/ui/memory", Dests: []navDest{
			{Key: "memory", Label: "Memory", Href: "/ui/memory", Icon: "navIconMemory"},
			{Key: "reminders", Label: "Reminders", Href: "/ui/reminders", Icon: "navIconReminders"},
		}},
		{Key: "insight", Label: "Insight", Icon: "navIconInsight", Href: "/ui/spend", Dests: []navDest{
			{Key: "spend", Label: "Spend", Href: "/ui/spend", Icon: "navIconSpend"},
			{Key: "trends", Label: "Trends", Href: "/ui/insights/trends", Icon: "navIconInsight"},
			{Key: "insights", Label: "Tool budget", Href: "/ui/insights/tool-budget", Icon: "navIconGauge"},
			{Key: "trading", Label: "Trading", Href: "/ui/trading", Icon: "navIconTrading", Cap: "trading"},
			{Key: "audit", Label: "Audit", Href: "/ui/audit", Icon: "navIconAudit", AdminOnly: true},
			{Key: "mcp", Label: "MCP", Href: "/ui/mcp", Icon: "navIconMcp", AdminOnly: true},
		}},
		{Key: "admin", Label: "Admin", Icon: "navIconAdmin", Href: "/ui/admin/", AdminOnly: true, Dests: []navDest{
			{Key: "admin", Label: "Admin console", Href: "/ui/admin/", Icon: "navIconAdmin"},
		}},
	}
}

// pageToArea is derived from navModel at init so the mapping cannot
// drift from the IA defined above.
var pageToArea = func() map[string]string {
	m := map[string]string{}
	for _, a := range navModel() {
		for _, d := range a.Dests {
			m[d.Key] = a.Key
		}
	}
	return m
}()

// navAreaForPage returns the top-level area key for a CurrentPage token, or
// "" when the page maps to no area (e.g. the dashboard, reached via the logo).
// "" means: no rail icon highlighted and no contextual panel open — so pages
// outside the IA don't leave a stale submenu hanging open.
func navAreaForPage(page string) string {
	if a, ok := pageToArea[page]; ok {
		return a
	}
	return ""
}
