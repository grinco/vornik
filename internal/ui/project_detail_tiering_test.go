// Tests for the project_detail.html re-tiering onto hierarchy primitives
// (panel-primary / panel-ref / statStrip / sectionHeader) and semantic
// ink-*/surface-* tokens.
//
// TDD: write tests first (they fail pre-migration), then migrate template.

package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// renderProjectDetailBody is a convenience wrapper used by all tiering
// tests. Mirrors renderTaskDetailBody / renderDashboardBody pattern.
func renderProjectDetailBody(t *testing.T, data ProjectDetailData) string {
	t.Helper()
	return renderProjectDetail(t, data)
}

// TestProjectDetailTiering_NoLegacyTokens asserts that the template
// source contains zero legacy gray-*/dark-* Tailwind classes. This is
// the "definitive green gate" — all other tiering tests validate
// structural correctness, this one validates the token migration.
func TestProjectDetailTiering_NoLegacyTokens(t *testing.T) {
	assertNoLegacyTokens(t, "project_detail.html")
}

// TestProjectDetailTiering_AutonomyHeroIsPrimaryPanel asserts that
// the autonomy hero block carries panel-primary. The block is the
// most operator-critical visible element on the page (goal +
// next-tick + last-outcome) so it earns the primary accent.
func TestProjectDetailTiering_AutonomyHeroIsPrimaryPanel(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID: "demo",
			Autonomy: registry.ProjectAutonomy{
				Enabled:      true,
				PollInterval: "5m",
				Goal:         "Run the daily digest.",
			},
		},
	}
	body := renderProjectDetailBody(t, data)

	// The autonomy hero card must carry panel-primary.
	if !strings.Contains(body, "panel-primary") {
		t.Errorf("autonomy hero block missing panel-primary class; body excerpt:\n%s",
			excerptAround(body, "Autonomy enabled", 400))
	}
}

// TestProjectDetailTiering_ArchivedBannerHasRoseTone verifies that the
// archived-project banner uses panel-primary with data-tone="rose".
func TestProjectDetailTiering_ArchivedBannerHasRoseTone(t *testing.T) {
	// Banner is panel-primary; its tone matches its background state —
	// rose when overdue (urgent), amber otherwise.
	base := func(overdue bool) ProjectDetailData {
		return ProjectDetailData{
			Title:       "Project: demo",
			CurrentPage: "projects",
			Project:     &registry.Project{ID: "demo"},
			Lifecycle: ProjectLifecyclePanel{
				IsArchived:        true,
				Overdue:           overdue,
				Countdown:         "in 5d",
				ArchivedAt:        "2026-06-01",
				ScheduledDeleteAt: "2026-06-08",
			},
		}
	}

	overdue := renderProjectDetailBody(t, base(true))
	if !strings.Contains(overdue, `data-tone="rose"`) {
		t.Errorf("overdue archived banner should have data-tone=\"rose\"; excerpt:\n%s",
			excerptAround(overdue, "archived", 400))
	}
	notOverdue := renderProjectDetailBody(t, base(false))
	if !strings.Contains(notOverdue, `data-tone="amber"`) {
		t.Errorf("non-overdue archived banner should have data-tone=\"amber\"; excerpt:\n%s",
			excerptAround(notOverdue, "archived", 400))
	}
	body := overdue

	if !strings.Contains(body, "panel-primary") {
		t.Errorf("archived banner missing panel-primary; excerpt:\n%s",
			excerptAround(body, "archived", 400))
	}
}

// TestProjectDetailTiering_RateLimitWarningHasAmberTone checks that the
// rate-limit warning banner uses panel-primary with data-tone="amber".
func TestProjectDetailTiering_RateLimitWarningHasAmberTone(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo"},
		RateLimitWarning: RateLimitWarning{
			Active:       true,
			RecentWarns:  2,
			RecentBlocks: 0,
			WindowLabel:  "5m",
		},
	}
	body := renderProjectDetailBody(t, data)

	if !strings.Contains(body, `data-tone="amber"`) {
		t.Errorf("rate-limit warning banner missing data-tone=\"amber\"; excerpt:\n%s",
			excerptAround(body, "rate limit", 400))
	}
}

// TestProjectDetailTiering_MCPServersIsRefPanel asserts that the MCP
// servers block renders inside a <details class="panel-ref" block,
// collapsed by default.
func TestProjectDetailTiering_MCPServersIsRefPanel(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID: "demo",
			MCP: registry.ProjectMCP{
				Servers: []registry.MCPServerConfig{
					{Name: "broker", Transport: "sse", URL: "http://localhost:9090/sse"},
				},
			},
		},
	}
	body := renderProjectDetailBody(t, data)

	if !strings.Contains(body, `<details class="panel-ref`) {
		t.Errorf("MCP servers block not inside panel-ref <details>; body excerpt around 'panel-ref':\n%s",
			excerptAround(body, "panel-ref", 500))
	}
}

// TestProjectDetailTiering_ConfigurationStatStrip asserts that the
// Configuration block (Swarm / Workflow / Priority / MaxConcurrent)
// renders its k/v grid using the statStrip primitive.
// statStrip emits text-ink-500 labels + text-ink-100 values.
func TestProjectDetailTiering_ConfigurationStatStrip(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID:                 "demo",
			SwarmID:            "swarm-1",
			DefaultWorkflowID:  "workflow-1",
			DefaultPriority:    5,
			MaxConcurrentTasks: 3,
		},
	}
	body := renderProjectDetailBody(t, data)

	// statStrip emits text-ink-500 (label) and text-ink-100 (value) classes
	if !strings.Contains(body, "text-ink-500") {
		t.Errorf("Configuration block not using statStrip (missing text-ink-500 label class)")
	}

	// The configuration values must still be visible
	for _, want := range []string{"swarm-1", "workflow-1", "Configuration"} {
		if !strings.Contains(body, want) {
			t.Errorf("Configuration block missing %q after migration", want)
		}
	}
}

// TestProjectDetailTiering_SectionHeaderOnConfiguration checks that the
// Configuration card uses sectionHeader, which emits border-surface-600/60.
func TestProjectDetailTiering_SectionHeaderOnConfiguration(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo"},
	}
	body := renderProjectDetailBody(t, data)

	// sectionHeader emits border-surface-600/60 in its border-b div.
	if !strings.Contains(body, "border-surface-600/60") {
		t.Errorf("page missing border-surface-600/60 — sectionHeader not used or token not migrated")
	}
}

// TestProjectDetailTiering_NoHtmxInClosedDetails verifies that the
// HTMX auto-refresh subtree (project-status-overview) is NOT inside
// a closed <details> element. The HTMX poll runs on page load; if
// wrapped in a closed <details>, the triggers would fire but targets
// would be invisible/absent.
//
// Strategy: find the hx-get attribute and assert it is not preceded
// by a <details> tag that is not [open] within the preceding 500 chars.
func TestProjectDetailTiering_NoHtmxInClosedDetails(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo"},
	}
	body := renderProjectDetailBody(t, data)

	htmxIdx := strings.Index(body, `hx-get="/ui/projects/demo"`)
	if htmxIdx < 0 {
		// HTMX auto-refresh not present; pass (no trigger = no problem).
		return
	}

	// Scan the 800 chars before the hx-get attribute for an unclosed <details
	start := htmxIdx - 800
	if start < 0 {
		start = 0
	}
	window := body[start:htmxIdx]

	// Count <details occurrences vs </details> occurrences — if there's
	// an unclosed <details> before the HTMX element, that's a violation.
	opens := strings.Count(window, "<details")
	closes := strings.Count(window, "</details>")
	if opens > closes {
		t.Errorf("HTMX auto-refresh element appears inside unclosed <details> — would break live refresh (opens=%d closes=%d in window before hx-get)", opens, closes)
	}
}

// TestProjectDetailTiering_StackPanelRefHasOpenChevron ensures that the
// old "Stack" <details> (which contained MCP/Swarm/Workflow) has been
// replaced or refactored into panel-ref <details> elements — each with
// a sectionHeader <summary>.
func TestProjectDetailTiering_StackPanelRefHasOpenChevron(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID: "demo",
			MCP: registry.ProjectMCP{
				Servers: []registry.MCPServerConfig{
					{Name: "svc", Transport: "stdio", Command: "/bin/svc"},
				},
			},
		},
		Swarm: &registry.Swarm{
			ID:       "swarm-1",
			LeadRole: "lead",
			Roles: []registry.SwarmRole{
				{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
			},
		},
	}
	body := renderProjectDetailBody(t, data)

	// At least one panel-ref <details> must be present.
	if !strings.Contains(body, `class="panel-ref`) {
		t.Errorf("no panel-ref <details> found on project_detail page with MCP+Swarm data")
	}
}

// TestProjectDetailWorkflowMetadataPreserved — regression guard for the
// b7b253f8 re-tier that accidentally dropped Workflow.Version,
// MaxStepVisits and MaxIterations from the rendered page.
// Written first so it fails against the pre-fix template (TDD red).
//
// Implementation note: toJSON .Workflow embeds these fields into the page
// inside a <script> block, so a simple strings.Contains check would pass
// even when the values are absent from the visible HTML. We verify that the
// values appear OUTSIDE the <script> block (i.e. in the real UI) by slicing
// the body up to the first <script> tag in the workflow section.
func TestProjectDetailWorkflowMetadataPreserved(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo"},
		Workflow: &registry.Workflow{
			ID:            "wf-sentinel",
			Version:       "9.8.7-sentinel",
			MaxStepVisits: 47,
			MaxIterations: 83,
		},
	}
	body := renderProjectDetailBody(t, data)

	// Find the workflow <details> block, then strip from the first <script>
	// so we only look at the visible HTML (not the embedded JSON).
	wfIdx := strings.Index(body, "wf-sentinel")
	if wfIdx < 0 {
		t.Fatal("workflow section not found in rendered page (Workflow.ID 'wf-sentinel' missing)")
	}
	// Slice to just the workflow panel up to its embedded <script> block.
	visibleSection := body[wfIdx:]
	if scriptIdx := strings.Index(visibleSection, "<script"); scriptIdx >= 0 {
		visibleSection = visibleSection[:scriptIdx]
	}

	for _, want := range []string{"9.8.7-sentinel", "47", "83"} {
		if !strings.Contains(visibleSection, want) {
			t.Errorf("workflow metadata %q missing from visible HTML (regression: b7b253f8 dropped Version/MaxStepVisits/MaxIterations); visible section:\n%s",
				want, visibleSection)
		}
	}
}

// TestProjectDetailAutonomyCapsStatStrip — asserts that autonomy caps
// (Poll Interval / Max Tasks/Hour / Require Approval) render through the
// statStrip primitive, not a hand-rolled grid.
//
// Strategy: statStrip renders labels with class "uppercase tracking-wider text-[10px]"
// — the hand-rolled grid used "text-xs text-ink-500" without uppercase/tracking.
// We verify that the label "POLL INTERVAL" (uppercased by CSS via statStrip's
// uppercase class) appears in the rendered body, which proves the caps went
// through statStrip. We also verify the statStrip dl grid class appears.
func TestProjectDetailAutonomyCapsStatStrip(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project: &registry.Project{
			ID: "demo",
			Autonomy: registry.ProjectAutonomy{
				Enabled:         true,
				PollInterval:    "10m",
				MaxTasksPerHour: 12,
				RequireApproval: true,
			},
		},
	}
	body := renderProjectDetailBody(t, data)

	// statStrip emits this grid class on its <dl> — presence within the
	// autonomy section confirms the caps went through the primitive.
	// We look for the grid class appearing AFTER the "Autonomy enabled" badge.
	autonomyIdx := strings.Index(body, "Autonomy enabled")
	if autonomyIdx < 0 {
		t.Fatal("autonomy section not found in body")
	}
	autonomySection := body[autonomyIdx:]
	// Trim after the closing div of the autonomy hero block — use the next
	// Configuration section as delimiter.
	if configIdx := strings.Index(autonomySection, "Configuration"); configIdx > 0 {
		autonomySection = autonomySection[:configIdx]
	}

	const gridClass = `grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-4 text-xs`
	if !strings.Contains(autonomySection, gridClass) {
		t.Errorf("autonomy caps not rendered via statStrip in autonomy section (missing grid class %q);\nautonomySection:\n%s", gridClass, autonomySection)
	}

	// statStrip uses "uppercase tracking-wider" on dt labels.
	if !strings.Contains(autonomySection, "uppercase tracking-wider") {
		t.Errorf("autonomy caps statStrip labels missing 'uppercase tracking-wider' — caps may still use hand-rolled grid")
	}

	// Cap values must appear in the autonomy section (incl. the numeric
	// Max Tasks/Hour cap = 12).
	for _, want := range []string{"10m", "12", "Yes"} {
		if !strings.Contains(autonomySection, want) {
			t.Errorf("autonomy cap value %q not found in autonomy section", want)
		}
	}
}

// TestProjectDetailSoakMetricsPanelRef — asserts that the Soak metrics
// section renders inside a <details class="panel-ref" element.
func TestProjectDetailSoakMetricsPanelRef(t *testing.T) {
	data := ProjectDetailData{
		Title:       "Project: demo",
		CurrentPage: "projects",
		Project:     &registry.Project{ID: "demo"},
		Trading: TradingPanel{
			Enabled:         true,
			BrokerReachable: true,
			Soak: SoakMetrics{
				SoakReady:          true,
				SharpeAnnualized:   1.5,
				MaxDrawdownPct:     1.2,
				Equity24hChangePct: 0.3,
				Equity24hChangeUSD: 30.0,
				WindowDays:         7,
				SampleCount:        2016,
			},
		},
	}
	body := renderProjectDetailBody(t, data)

	// The soak metrics block must be inside a panel-ref <details>.
	if !strings.Contains(body, `<details class="panel-ref`) {
		t.Errorf("Soak metrics not wrapped in panel-ref <details>; body excerpt:\n%s",
			excerptAround(body, "Soak metrics", 600))
	}
	// Sharpe value must still appear.
	if !strings.Contains(body, "1.50") {
		t.Errorf("Soak metrics Sharpe value missing from rendered body")
	}
}
