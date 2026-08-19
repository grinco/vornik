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
	// Badge is an optional live count rendered next to Label as "Label
	// (N)" (design outcome-inbox-design.md §5.7 Q4 — the "My requests
	// (3)" nav badge). Zero = no badge. navModel() itself (the
	// canonical IA below) never sets this; it is populated per-request
	// by navModelForPage, the "navModel" FuncMap entry NewServer wires
	// up, from the CURRENT page's Data struct via the opt-in
	// navAttentionCounter interface. A page whose Data doesn't
	// implement that interface simply renders no badge — deliberately
	// simple rather than threading live nav state through every one of
	// the ~40 page handlers' render() call.
	Badge int
}

// navAreaDef is one top-level area shown in the icon rail. AdminOnly
// areas are rendered hidden-by-default and unhidden by the same
// IsAdmin / marker-cookie path used for the legacy Admin link.
type navAreaDef struct {
	Key   string
	Label string
	// Short is the compact label the < md bottom tab bar renders in place
	// of Label (2026-07-10 mobile-nav-overflow fix): the bar packs up to 8
	// flex-1 cells (~47px each on a 375px phone), where a 10px-font label
	// longer than ~7 characters ("Orchestration", "Integrations") overflows
	// its cell. Empty = Label is already short enough; see MobileLabel.
	Short string
	Icon  string
	// Href is the rail icon's click target — the area's primary
	// destination. Kept as an explicit field (not index .Dests 0) because
	// the template FuncMap overrides the builtin `index`.
	Href      string
	AdminOnly bool
	Dests     []navDest
}

// MobileLabel is what the < md bottom tab bar renders under the area's
// icon: Short when set, else Label. Desktop surfaces (rail tooltip,
// contextual panel, drawer heading) always use the full Label.
func (a navAreaDef) MobileLabel() string {
	if a.Short != "" {
		return a.Short
	}
	return a.Label
}

// navModel is the single source of truth for the navigation IA. The
// icon rail, the contextual panel, and the mobile drawer all render
// from this slice so they cannot drift apart.
//
// The full model includes every destination; edition/capability gating is
// applied by navModelFunc (e.g. Trading is dropped on Community). Keeping
// navModel itself pure means pageToArea and tests see the canonical IA.
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
			// Relabelled from "Needs you" to "My requests" (task 4.4,
			// design §5.7): the inbox is now the non-admin default home
			// and also carries the broader "Your requests" list (any
			// status, not just what needs attention), so the nav label
			// needed to widen with it. Not role-aware (admins see the
			// same label) — kept simple per the design's own hedge; a
			// per-role label would need the same per-request plumbing
			// problem Badge below already had to solve narrowly.
			{Key: "inbox", Label: "My requests", Href: "/ui/inbox", Icon: "navIconInbox"},
		}},
		// Tasks leads the Orchestration area: it's where the operator most
		// often works, so it's the default destination (the rail icon lands
		// here). Projects (the container for swarms/workflows/tasks), Swarms,
		// Workflows, and Executions follow. Each dest points at its
		// first-class top-level list page (IA completion, 2026-06-09):
		// Swarms/Workflows list the global registry entities; Executions is
		// the cross-task run list. Row click-through reaches the existing
		// detail/edit surfaces.
		{Key: "orchestration", Label: "Orchestration", Short: "Tasks", Icon: "navIconOrchestration", Href: "/ui/tasks", Dests: []navDest{
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
		// Integrations Hub (design doc: integrations-hub-design.md, task
		// 5.3) — "connect my tools" is a distinct user job from Admin (which
		// RoleUser cannot see), so it gets its own top-level area rather than
		// folding into admin. No AdminOnly flag: the catalog itself is
		// scope-filtered (daemon-scope kinds hidden for non-admins, §5.8),
		// so a project-scoped user sees a non-empty, relevant page here.
		{Key: "integrations", Label: "Integrations", Short: "Connect", Icon: "navIconIntegrations", Href: "/ui/integrations", Dests: []navDest{
			{Key: "integrations", Label: "Integrations", Href: "/ui/integrations", Icon: "navIconIntegrations"},
		}},
		{Key: "insight", Label: "Insight", Icon: "navIconInsight", Href: "/ui/spend", Dests: []navDest{
			{Key: "spend", Label: "Spend", Href: "/ui/spend", Icon: "navIconSpend"},
			{Key: "trends", Label: "Trends", Href: "/ui/insights/trends", Icon: "navIconInsight"},
			{Key: "quality", Label: "Quality", Href: "/ui/insights/quality", Icon: "navIconScorecard"},
			{Key: "insights", Label: "Tool budget", Href: "/ui/insights/tool-budget", Icon: "navIconGauge"},
			{Key: "adoption", Label: "Adoption", Href: "/ui/insights/adoption", Icon: "navIconAdoption"},
			{Key: "trading", Label: "Trading", Href: "/ui/trading", Icon: "navIconTrading", Cap: "trading"},
			{Key: "audit", Label: "Audit", Href: "/ui/audit", Icon: "navIconAudit", AdminOnly: true},
			// MCP moved to the control-plane hub's MCP tab (the canonical
			// management surface). The old top-level /ui/mcp discovery page
			// was a duplicate nav entry point; it now 302s into the hub
			// (2026-07-08 nav dedupe).
		}},
		{Key: "admin", Label: "Admin", Icon: "navIconAdmin", Href: "/ui/admin/", AdminOnly: true, Dests: []navDest{
			{Key: "admin", Label: "Admin console", Href: "/ui/admin/", Icon: "navIconAdmin"},
			{Key: "admin-skills", Label: "Skills", Href: "/ui/admin/skills", Icon: "navIconSkill"},
			{Key: "admin-control-plane", Label: "Control plane", Href: "/ui/admin/control-plane", Icon: "navIconControlPlane"},
			{Key: "admin-keys", Label: "Keys & access", Href: "/ui/admin/keys", Icon: "navIconKey"},
		}},
	}
}

// navModelFunc is the capability-aware nav template func. It drops the
// Trading destination unless tradingEnabled() reports the destination is
// worth showing — without this the data-cap hint would still render a dead
// or empty link. Built at the template-setup site so uiFuncMap stays
// edition-agnostic.
//
// tradingEnabled is a predicate, not a bool, because the answer is not fixed
// at server-construction time: it combines the static edition gate with a
// live "does any project actually have trading configured" check that must
// follow a config reload (see Server.tradingNavEnabled). It is called on
// every nav render; nil means "hidden".
func navModelFunc(tradingEnabled func() bool) func() []navAreaDef {
	return func() []navAreaDef {
		m := navModel()
		if tradingEnabled != nil && tradingEnabled() {
			return m
		}
		for i := range m {
			m[i].Dests = filterDests(m[i].Dests, "trading")
		}
		return m
	}
}

// filterDests returns dests with any entry whose Key == drop removed,
// preserving order. Used to elide edition-gated destinations (e.g. Trading
// on Community) so the nav never renders a link to a 404 route.
func filterDests(dests []navDest, drop string) []navDest {
	out := make([]navDest, 0, len(dests))
	for _, d := range dests {
		if d.Key != drop {
			out = append(out, d)
		}
	}
	return out
}

// pageToArea is derived from navModel at init so the mapping cannot
// drift from the IA defined above.
var pageToArea = func() map[string]string {
	m := map[string]string{}
	// Map every page→area including Trading: this is only consulted when the
	// caller is actually on that page (which can't happen in CE, where the
	// route 404s), and keeping it edition-independent avoids a second flag here.
	for _, a := range navModel() {
		for _, d := range a.Dests {
			m[d.Key] = a.Key
		}
	}
	return m
}()

// navAttentionCounter lets a page's Data struct expose the viewer's live
// outcome-inbox attention count for the nav's "My requests" badge
// (design outcome-inbox-design.md §5.7 Q4), without threading per-request
// nav state through every page handler's render() call. Optional/opt-in:
// a Data struct that doesn't implement it (every page except InboxData,
// as of task 4.4) simply renders no badge.
type navAttentionCounter interface {
	NavAttentionCount() int
}

// navModelForPage is the template FuncMap's "navModel" entry (wired by
// NewServer in place of the bare navModelFunc). It returns the same
// edition-filtered destinations navModelFunc does, plus the "inbox"
// dest's Badge populated from data's attention count when data
// implements navAttentionCounter. _partials.html's nav partial calls
// this as `{{range navModel .}}` (four call sites — rail, panel, mobile
// tabs, mobile drawer) so `.` (the current page's Data) reaches here.
//
// base() is called fresh on every invocation (mirroring navModelFunc's
// existing per-render allocation), so mutating the returned slice's
// Dests in place below is safe — it never aliases another request's
// copy.
func navModelForPage(tradingEnabled func() bool) func(data any) []navAreaDef {
	base := navModelFunc(tradingEnabled)
	return func(data any) []navAreaDef {
		m := base()
		counter, ok := data.(navAttentionCounter)
		if !ok {
			return m
		}
		count := counter.NavAttentionCount()
		if count <= 0 {
			return m
		}
		for i := range m {
			for j := range m[i].Dests {
				if m[i].Dests[j].Key == "inbox" {
					m[i].Dests[j].Badge = count
				}
			}
		}
		return m
	}
}

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
