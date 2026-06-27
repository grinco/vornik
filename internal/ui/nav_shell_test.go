package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNavHrefsAreRoutable guards the nav IA against dangling links: every
// navModel destination (and each area's rail Href) must resolve to a
// registered route — never a 404. The UI mux is mounted under /ui at a higher
// level, so Handler() serves bare paths; strip the /ui prefix here. Auth/data
// gaps may yield 401/403/500 in this bare test server — those still mean the
// route EXISTS; only 404 is a dangling nav link.
func TestNavHrefsAreRoutable(t *testing.T) {
	// Enable the capability-gated routes so their nav hrefs resolve — the nav
	// lists Trading (Cap "trading") and /trading is edition-gated (2026-06-27),
	// so the routability guard must test a capability-enabled deployment.
	s := NewServer(WithTradingEnabled())
	h := s.Handler()
	seen := map[string]bool{}
	check := func(href string) {
		if href == "" || seen[href] {
			return
		}
		seen[href] = true
		path := strings.TrimPrefix(href, "/ui")
		if path == "" {
			path = "/"
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// Follow a single trailing-slash redirect (Go's mux emits 307/308
		// for subtree-root-without-slash) so we test the real destination.
		switch rec.Code {
		case http.StatusMovedPermanently, http.StatusFound,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := rec.Header().Get("Location")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, loc, nil))
		}
		if rec.Code == http.StatusNotFound {
			t.Errorf("nav href %q (path %q) is a dangling link (404)", href, path)
		}
	}
	for _, area := range navModel() {
		check(area.Href)
		for _, d := range area.Dests {
			check(d.Href)
		}
	}
}

// navFixture is a struct (not a map) because hasAdminFlag/hasSessionFlag are
// reflection helpers that only read struct fields.
type navFixture struct {
	CurrentPage string
	IsAdmin     bool
	IsSession   bool
}

func TestDesktopNavRendersRailAndPanel(t *testing.T) {
	html := renderTemplateString(t, "nav", navFixture{CurrentPage: "workflows", IsAdmin: true, IsSession: true})
	for _, label := range []string{"Orchestration", "Memory", "Insight", "Admin"} {
		if !strings.Contains(html, label) {
			t.Errorf("rail missing area %q", label)
		}
	}
	if !strings.Contains(html, "/ui/swarms") || !strings.Contains(html, "/ui/workflows") {
		t.Error("orchestration panel missing swarms/workflows destinations")
	}
	if !strings.Contains(html, `aria-current="page"`) {
		t.Error("active destination not marked aria-current")
	}
	if strings.Count(html, "glass") < 2 {
		t.Error("rail and panel should both carry the glass class")
	}
	if !strings.Contains(html, `href="#main"`) {
		t.Error("skip-to-main link removed")
	}
}

func TestDesktopNavHidesAdminForNonAdmin(t *testing.T) {
	html := renderTemplateString(t, "nav", navFixture{CurrentPage: "dashboard", IsAdmin: false})
	if !strings.Contains(html, "data-admin-link") {
		t.Error("admin rail item must keep data-admin-link for marker-cookie unhide")
	}
}

// TestNavShellAccessibility — Phase 0 a11y guard: motion gated behind
// prefers-reduced-motion, active rail area + panel destination both carry
// aria-current, and the skip-to-main link survives the shell rewrite.
func TestNavShellAccessibility(t *testing.T) {
	head := renderTemplateString(t, "pageHead", map[string]any{"Title": "T"})
	if !strings.Contains(head, "prefers-reduced-motion") {
		t.Error("nav shell motion not gated behind prefers-reduced-motion")
	}
	nav := renderTemplateString(t, "nav", navFixture{CurrentPage: "swarms", IsAdmin: true})
	if !strings.Contains(nav, `aria-current="true"`) {
		t.Error("active rail area not marked aria-current")
	}
	if !strings.Contains(nav, `aria-current="page"`) {
		t.Error("active panel destination not marked aria-current")
	}
	if !strings.Contains(nav, `href="#main"`) {
		t.Error("skip-to-main link missing after shell rewrite")
	}
}

func TestMobileNavTabsAndDrawer(t *testing.T) {
	html := renderTemplateString(t, "nav", navFixture{CurrentPage: "swarms", IsSession: true})
	if !strings.Contains(html, "md:hidden") {
		t.Error("mobile bottom bar missing")
	}
	for _, label := range []string{"Orchestration", "Memory", "Insight"} {
		if !strings.Contains(html, label) {
			t.Errorf("mobile tabs missing area %q", label)
		}
	}
	if !strings.Contains(html, "data-nav-drawer") {
		t.Error("mobile contextual drawer container missing")
	}
	if !strings.Contains(html, "safe-pb") {
		t.Error("safe-area padding removed")
	}
}

// TestShellOffsetCSSDefined guards the overlay fix: the offset must be a real
// CSS rule in pageHead (the Tailwind Play CDN didn't reliably JIT the
// `md:pl-[16.5rem]` arbitrary value, leaving the panel overlaying content).
func TestShellOffsetCSSDefined(t *testing.T) {
	head := renderTemplateString(t, "pageHead", map[string]any{"Title": "T"})
	// Body-level offset (not a per-main class — px-*/mx-auto on <main> win
	// the cascade). The fixed rail ignores body padding and stays pinned.
	if !strings.Contains(head, `body:has(nav[aria-label="Primary"])`) {
		t.Error("pageHead missing the body-level shell offset rule — pages render under the fixed rail+panel")
	}
	// Rail-only offset (3.5rem); the contextual panel is a hover flyout that
	// overlays content rather than reserving width.
	if !strings.Contains(head, "padding-left: 3.5rem") {
		t.Error("shell offset padding-left missing")
	}
}

// TestPageFooterRemoved — the redundant brand-colour footer was removed; the
// define renders nothing.
func TestPageFooterRemoved(t *testing.T) {
	out := renderTemplateString(t, "pageFooter", nil)
	if strings.Contains(out, "<footer") {
		t.Errorf("pageFooter should render nothing; got: %q", out)
	}
}

func TestNavIconsDefined(t *testing.T) {
	for _, name := range []string{
		"navIconOverview", "navIconOrchestration", "navIconSwarms",
		"navIconWorkflows", "navIconExecutions", "navIconTasks",
		"navIconMemory", "navIconReminders", "navIconInsight",
		"navIconSpend", "navIconAudit", "navIconMcp", "navIconAdmin",
	} {
		out := renderTemplateString(t, name, nil)
		if !strings.Contains(out, "<svg") {
			t.Errorf("icon define %q did not render an <svg>", name)
		}
	}
}

// dashboardTestData returns the minimal DashboardData the template
// needs to render without panicking. Zero-value structs render their
// empty-state branches; the custom `index` funcmap entry handles the
// nil TaskCounts map. Wave-1 re-skin guard.
func dashboardTestData() DashboardData {
	return DashboardData{
		Title:       "Dashboard",
		CurrentPage: "dashboard",
	}
}

// TestDashboardConsumesShellOffsetAndGlass — wave-1 re-skin guard.
// The dashboard's <main> must clear the fixed rail+panel on md+ and
// its cards must ride the glass surface system. Renders the production
// template via the shared helper. The template define is
// "dashboard.html"; renderTemplateString resolves by that name.
func TestDashboardConsumesShellOffsetAndGlass(t *testing.T) {
	html := renderTemplateString(t, "dashboard.html", dashboardTestData())
	if !strings.Contains(html, "shell-offset") {
		t.Error("dashboard main does not clear the rail+panel")
	}
	if !strings.Contains(html, "glass") {
		t.Error("dashboard cards not migrated to glass")
	}
}

// TestWave2ConsumesShellOffsetAndGlass — wave-2 re-skin guard for the
// memory & projects pages. projects.html renders cleanly from a
// zero-value ProjectsData (empty Rows hits the empty-state branch),
// which still emits the offset <main> and a glass empty-state card.
// The define name is the filename ("projects.html"); renderTemplateString
// resolves by that name.
func TestWave2ConsumesShellOffsetAndGlass(t *testing.T) {
	html := renderTemplateString(t, "projects.html", ProjectsData{
		Title:       "Projects",
		CurrentPage: "projects",
	})
	if !strings.Contains(html, "shell-offset") {
		t.Error("projects main does not clear the rail+panel")
	}
	if !strings.Contains(html, "glass") {
		t.Error("projects cards not migrated to glass")
	}
}

// TestWave3ConsumesShellOffsetAndGlass — wave-3 re-skin guard for the
// insight & misc pages. audit.html renders cleanly from a zero-value
// AuditData (empty Entries/WebhookEvents hit the empty-state branches),
// which still emits the offset <main> and glass empty-state cards. The
// define name is the filename ("audit.html"); renderTemplateString
// resolves by that name.
func TestWave3ConsumesShellOffsetAndGlass(t *testing.T) {
	html := renderTemplateString(t, "audit.html", AuditData{
		Title:       "Audit Log",
		CurrentPage: "audit",
	})
	if !strings.Contains(html, "shell-offset") {
		t.Error("audit main does not clear the rail+panel")
	}
	if !strings.Contains(html, "glass") {
		t.Error("audit cards not migrated to glass")
	}
}

// TestWave4ConsumesShellOffsetAndGlass — wave-4 re-skin guard for the
// admin pages. admin_health_index.html renders cleanly from a zero-value
// AdminHealthIndexData (it only embeds adminCommonData; the page body is
// static link cards), and still emits the offset <main> and glass cards.
// The define name is the filename ("admin_health_index.html");
// renderTemplateString resolves by that name.
func TestWave4ConsumesShellOffsetAndGlass(t *testing.T) {
	html := renderTemplateString(t, "admin_health_index.html", AdminHealthIndexData{
		adminCommonData: adminCommonData{
			Title:       "Admin Health",
			CurrentPage: "admin",
			IsAdmin:     true,
		},
	})
	if !strings.Contains(html, "shell-offset") {
		t.Error("admin health main does not clear the rail+panel")
	}
	if !strings.Contains(html, "glass") {
		t.Error("admin health cards not migrated to glass")
	}
}

func TestPageHeadHasGlassTokensAndDarkDefault(t *testing.T) {
	html := renderTemplateString(t, "pageHead", map[string]any{"Title": "Test"})
	// Glass tokens defined.
	for _, tok := range []string{"--glass-bg", "--glass-border", "--glass-blur"} {
		if !strings.Contains(html, tok) {
			t.Errorf("pageHead missing glass token %s", tok)
		}
	}
	// Reduced-transparency + backdrop-filter fallbacks present.
	if !strings.Contains(html, "prefers-reduced-transparency") {
		t.Error("pageHead missing prefers-reduced-transparency fallback")
	}
	if !strings.Contains(html, "@supports not (backdrop-filter") {
		t.Error("pageHead missing backdrop-filter @supports fallback")
	}
	// Dark is the default: the early-paint resolver queries for an explicit
	// LIGHT preference and falls through to dark (was: query dark → light).
	if !strings.Contains(html, "(prefers-color-scheme: light)") {
		t.Error("early-paint resolver should default to dark (query prefers-color-scheme: light)")
	}
}
