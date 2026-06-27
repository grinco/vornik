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
		"projects":   "orchestration",
		"swarms":     "orchestration",
		"workflows":  "orchestration",
		"tasks":      "orchestration",
		"executions": "orchestration", // restored as a dest with the cross-task Executions list (IA completion)
		"memory":     "memory",
		"reminders":  "memory",
		"spend":      "insight",
		"trading":    "insight",
		"audit":      "insight",
		"mcp":        "insight",
		"admin":      "admin",
		"dashboard":  "", // reached via the logo; no area/panel
		"":           "", // unknown → no area (no stale panel)
		"nonsense":   "",
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
	wantAreas := []string{"steer", "orchestration", "memory", "insight", "admin"}
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
