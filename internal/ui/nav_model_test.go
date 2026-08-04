package ui

import (
	"html/template"
	"strings"
	"testing"
)

func TestNavHelpersRegistered(t *testing.T) {
	tmpl := template.New("t").Funcs(uiFuncMap())
	src := `{{$a := navAreaForPage "swarms"}}{{$a}}|{{range navModel}}{{.Key}} {{end}}`
	tmpl, err := tmpl.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "orchestration|") {
		t.Errorf("navAreaForPage not wired: %q", out)
	}
	if !strings.Contains(out, "orchestration") || !strings.Contains(out, "admin") {
		t.Errorf("navModel not wired: %q", out)
	}
}

func TestNavAreaForPage(t *testing.T) {
	cases := map[string]string{
		"projects":     "orchestration",
		"swarms":       "orchestration",
		"workflows":    "orchestration",
		"tasks":        "orchestration",
		"executions":   "orchestration", // restored as a dest with the cross-task Executions list (IA completion)
		"memory":       "memory",
		"reminders":    "memory",
		"integrations": "integrations",
		"spend":        "insight",
		"trading":      "insight",
		"audit":        "insight",
		"admin":        "admin",
		// Admin sub-destinations with dedicated panel items must map to the
		// admin area (2026-07-08 highlight fix — handlers previously passed
		// the generic "admin" token so these items never lit up).
		"admin-skills":        "admin",
		"admin-keys":          "admin",
		"admin-control-plane": "admin",
		"dashboard":           "", // reached via the logo; no area/panel
		"mcp":                 "", // removed from nav (2026-07-08 dedupe → hub MCP tab); no longer an area
		"":                    "", // unknown → no area (no stale panel)
		"nonsense":            "",
	}
	for page, want := range cases {
		if got := navAreaForPage(page); got != want {
			t.Errorf("navAreaForPage(%q) = %q, want %q", page, got, want)
		}
	}
}

func TestNavModelContract(t *testing.T) {
	m := navModel()
	// Areas in display order.
	wantAreas := []string{"steer", "orchestration", "memory", "integrations", "insight", "admin"}
	if len(m) != len(wantAreas) {
		t.Fatalf("navModel has %d areas, want %d", len(m), len(wantAreas))
	}
	for i, a := range m {
		if a.Key != wantAreas[i] {
			t.Errorf("area %d = %q, want %q", i, a.Key, wantAreas[i])
		}
	}
	// Swarms & Workflows are first-class under orchestration.
	var orch navAreaDef
	for _, a := range m {
		if a.Key == "orchestration" {
			orch = a
		}
	}
	// Tasks is the default (top) destination — it's where the operator
	// most often works. Executions follows Workflows: with a real
	// cross-task Executions list (IA completion) it's a first-class
	// destination again.
	wantDests := []string{"tasks", "projects", "swarms", "workflows", "executions"}
	if len(orch.Dests) != len(wantDests) {
		t.Fatalf("orchestration has %d dests, want %d", len(orch.Dests), len(wantDests))
	}
	for i, d := range orch.Dests {
		if d.Key != wantDests[i] {
			t.Errorf("orchestration dest %d = %q, want %q", i, d.Key, wantDests[i])
		}
		if d.Href == "" || d.Label == "" {
			t.Errorf("orchestration dest %q missing Href/Label", d.Key)
		}
	}
	// Steer is the new live-control area: Live + Needs-you, leading the rail.
	var steer navAreaDef
	for _, a := range m {
		if a.Key == "steer" {
			steer = a
		}
	}
	steerDests := []string{"live", "inbox"}
	if len(steer.Dests) != len(steerDests) {
		t.Fatalf("steer has %d dests, want %d", len(steer.Dests), len(steerDests))
	}
	for i, d := range steer.Dests {
		if d.Key != steerDests[i] {
			t.Errorf("steer dest %d = %q, want %q", i, d.Key, steerDests[i])
		}
	}
	if steer.Href != "/ui/live" {
		t.Errorf("steer area Href = %q, want /ui/live", steer.Href)
	}
	// The rail icon's primary target follows the default destination
	// (tasks), not projects.
	if orch.Href != "/ui/tasks" {
		t.Errorf("orchestration area Href = %q, want /ui/tasks (the default destination)", orch.Href)
	}
	// Admin area is flagged admin-only.
	for _, a := range m {
		if a.Key == "admin" && !a.AdminOnly {
			t.Error("admin area must be AdminOnly")
		}
	}
	// Trading is a first-class destination under Insight with a wired
	// Href/Label/Icon (the dashboard restored after the UI refactor).
	var insight navAreaDef
	for _, a := range m {
		if a.Key == "insight" {
			insight = a
		}
	}
	var trading navDest
	for _, d := range insight.Dests {
		if d.Key == "trading" {
			trading = d
		}
	}
	if trading.Key != "trading" {
		t.Fatal("insight area must include a trading destination")
	}
	if trading.Href != "/ui/trading" || trading.Label == "" || trading.Icon != "navIconTrading" {
		t.Errorf("trading dest mis-wired: %+v", trading)
	}
}

// navGate adapts a static bool to the func() bool predicate navModelFunc and
// navModelForPage take. Production passes Server.tradingNavEnabled, which is
// re-evaluated per render (see trading_nav_gate_test.go).
func navGate(v bool) func() bool { return func() bool { return v } }

// TestNavModelCommunityHidesTrading pins the 2026-06-29 fix: trading is an
// Enterprise-only capability (the /trading route 404s on CE via
// WithTradingEnabled), so a false gate must omit the Trading destination
// entirely — otherwise CE renders a nav link to a 404. A true gate keeps it,
// and the canonical navModel() is unchanged.
func TestNavModelCommunityHidesTrading(t *testing.T) {
	// Community: no "trading" dest anywhere.
	for _, a := range navModelFunc(navGate(false))() {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				t.Fatalf("navModelFunc(navGate(false)) must omit the Trading dest; found it under %q", a.Key)
			}
		}
	}
	// Enterprise keeps it (guards against an over-eager filter).
	found := false
	for _, a := range navModelFunc(navGate(true))() {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("navModelFunc(navGate(true)) must keep the Trading dest")
	}
	// Canonical navModel() is edition-agnostic — always full.
	if !navModelHasTrading(navModel()) {
		t.Fatal("navModel() must include the Trading dest (gating happens in navModelFunc)")
	}

	// Render-level: the CE-wired navModel func emits no trading entry; sibling
	// Insight dests (spend) still render.
	fm := uiFuncMap()
	fm["navModel"] = navModelFunc(navGate(false))
	tmpl, err := template.New("t").Funcs(fm).
		Parse(`{{range navModel}}{{range .Dests}}{{.Key}} {{end}}{{end}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := sb.String()
	if strings.Contains(out, "trading") {
		t.Errorf("CE nav render must not contain trading: %q", out)
	}
	if !strings.Contains(out, "spend") {
		t.Errorf("CE nav render should still contain sibling Insight dests: %q", out)
	}
}

// TestNavModel_InboxRelabelledMyRequests pins the task 4.4 relabel
// (design §5.7): the "Needs you" nav destination becomes "My requests"
// now that the inbox is the non-admin default home and carries the
// broader "Your requests" list, not just what needs attention.
func TestNavModel_InboxRelabelledMyRequests(t *testing.T) {
	var steer navAreaDef
	for _, a := range navModel() {
		if a.Key == "steer" {
			steer = a
		}
	}
	var inbox navDest
	for _, d := range steer.Dests {
		if d.Key == "inbox" {
			inbox = d
		}
	}
	if inbox.Label != "My requests" {
		t.Errorf("inbox dest Label = %q, want %q", inbox.Label, "My requests")
	}
}

// --- navModelForPage: the per-request "My requests (N)" badge (task
// 4.4, design §5.7 Q4) ---

// fakeNavCounter is a minimal navAttentionCounter for testing
// navModelForPage without depending on InboxData.
type fakeNavCounter struct{ n int }

func (f fakeNavCounter) NavAttentionCount() int { return f.n }

func TestNavModelForPage_BadgesInboxWhenCounterPositive(t *testing.T) {
	m := navModelForPage(navGate(true))(fakeNavCounter{n: 3})
	var got int
	found := false
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key == "inbox" {
				got = d.Badge
				found = true
			}
		}
	}
	if !found {
		t.Fatal("inbox dest not found")
	}
	if got != 3 {
		t.Errorf("inbox dest Badge = %d, want 3", got)
	}
}

// TestNavModelForPage_NoCounterNoBadge — a page whose Data doesn't
// implement navAttentionCounter (every page except InboxData, as of
// task 4.4) renders no badge — "keep it simple" per the design's hedge.
func TestNavModelForPage_NoCounterNoBadge(t *testing.T) {
	for _, data := range []any{nil, "a plain string", struct{ Foo string }{Foo: "bar"}} {
		m := navModelForPage(navGate(true))(data)
		for _, a := range m {
			for _, d := range a.Dests {
				if d.Key == "inbox" && d.Badge != 0 {
					t.Errorf("data=%#v: inbox dest Badge = %d, want 0 (no counter implemented)", data, d.Badge)
				}
			}
		}
	}
}

// TestNavModelForPage_ZeroCounterNoBadge — an implementer reporting 0
// (or negative) attention items must not render "(0)".
func TestNavModelForPage_ZeroCounterNoBadge(t *testing.T) {
	m := navModelForPage(navGate(true))(fakeNavCounter{n: 0})
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key == "inbox" && d.Badge != 0 {
				t.Errorf("inbox dest Badge = %d, want 0 for a zero count", d.Badge)
			}
		}
	}
}

// TestNavModelForPage_OnlyInboxDestBadged — the counter must never leak
// onto an unrelated destination (e.g. "tasks").
func TestNavModelForPage_OnlyInboxDestBadged(t *testing.T) {
	m := navModelForPage(navGate(true))(fakeNavCounter{n: 5})
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key != "inbox" && d.Badge != 0 {
				t.Errorf("dest %q got Badge = %d, want 0 (only inbox should badge)", d.Key, d.Badge)
			}
		}
	}
}

// TestInboxData_NavAttentionCount — InboxData implements
// navAttentionCounter via its own Count field.
func TestInboxData_NavAttentionCount(t *testing.T) {
	d := InboxData{Count: 7}
	var counter navAttentionCounter = d
	if got := counter.NavAttentionCount(); got != 7 {
		t.Errorf("NavAttentionCount() = %d, want 7", got)
	}
}

func navModelHasTrading(m []navAreaDef) bool {
	for _, a := range m {
		for _, d := range a.Dests {
			if d.Key == "trading" {
				return true
			}
		}
	}
	return false
}

// TestNavModel_MobileLabelsFitTabBar is the 2026-07-10 mobile-nav-overflow
// regression: the < md bottom bar renders up to 8 flex-1 cells (Home + 6
// areas + Sign out), which leaves ~47px per cell on a 375px phone. A
// 10px-font label wider than ~7 characters ("Orchestration",
// "Integrations") overflows its cell, so every area must expose a mobile
// label that fits; the template renders MobileLabel(), not Label, in the
// tab bar.
func TestNavModel_MobileLabelsFitTabBar(t *testing.T) {
	const maxMobileLabelLen = 7
	for _, a := range navModel() {
		got := a.MobileLabel()
		if got == "" {
			t.Errorf("area %q: MobileLabel() is empty", a.Key)
		}
		if len(got) > maxMobileLabelLen {
			t.Errorf("area %q: MobileLabel() = %q (%d chars), want <= %d — it overflows the mobile tab bar",
				a.Key, got, len(got), maxMobileLabelLen)
		}
	}
}

// MobileLabel falls back to Label when no Short label is set, so a future
// area with an already-short Label needs no extra field.
func TestNavAreaDef_MobileLabelFallsBackToLabel(t *testing.T) {
	a := navAreaDef{Label: "Steer"}
	if got := a.MobileLabel(); got != "Steer" {
		t.Errorf("MobileLabel() = %q, want the Label fallback %q", got, "Steer")
	}
	a = navAreaDef{Label: "Orchestration", Short: "Tasks"}
	if got := a.MobileLabel(); got != "Tasks" {
		t.Errorf("MobileLabel() = %q, want the Short override %q", got, "Tasks")
	}
}
