package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestNavPartial_IsAdminFlag locks the hidden-nav rule: when the
// rendered template data carries IsAdmin=true the partial emits
// the "Admin" link visible; otherwise the link is rendered with
// the `hidden` class (so the marker-cookie gate script can unhide
// it for session admins on pages whose data structs don't know the
// caller — see signout_nav_test.go).
//
// CONTRACT CHANGE 2026-06-06 (github-login follow-up): slice 1's
// "absent from the DOM" rule relaxed to "hidden by default". The
// invisibility property non-admins relied on is preserved (the
// link carries no data and the admin gate 403s server-side); what
// it buys is an Admin link that session admins can see on ALL ~40
// nav-bearing pages, not just admin pages.
func TestNavPartial_IsAdminFlag(t *testing.T) {
	s := NewServer()

	type fixture struct {
		IsAdmin     bool
		CurrentPage string
	}

	cases := []struct {
		name        string
		data        fixture
		wantContain string
		wantAbsent  string
	}{
		{
			// UI refresh 2026-06-08: nav is now an icon rail; the admin
			// item carries the rail-ico class (was flex) and no leading
			// `hidden` when the caller is an admin.
			name:        "IsAdmin=true renders Admin link visible",
			data:        fixture{IsAdmin: true, CurrentPage: "dashboard"},
			wantContain: `data-admin-link class="rail-ico`,
			wantAbsent:  `data-admin-link class="hidden `,
		},
		{
			name:        "IsAdmin=false renders Admin link hidden",
			data:        fixture{IsAdmin: false, CurrentPage: "dashboard"},
			wantContain: `data-admin-link class="hidden `,
			wantAbsent:  `data-admin-link class="rail-ico`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := s.templates.ExecuteTemplate(&buf, "nav", tc.data); err != nil {
				t.Fatalf("ExecuteTemplate(nav): %v", err)
			}
			rendered := buf.String()
			if tc.wantContain != "" && !strings.Contains(rendered, tc.wantContain) {
				t.Errorf("rendered nav should contain %q; got:\n%s", tc.wantContain, rendered)
			}
			if tc.wantAbsent != "" && strings.Contains(rendered, tc.wantAbsent) {
				t.Errorf("rendered nav should NOT contain %q; got:\n%s", tc.wantAbsent, rendered)
			}
		})
	}
}

// TestNavPartial_AdminOnlyDests locks the per-destination hidden-nav
// rule: shared/daemon-global surfaces (swarms, workflows, audit, mcp)
// a project-scoped RoleUser cannot reach are rendered hidden + tagged
// data-admin-link for non-admins, and visible for admins. Mirrors the
// area-level Admin-link rule. Prevents the UX gap where a non-admin saw
// nav links that 403 server-side.
func TestNavPartial_AdminOnlyDests(t *testing.T) {
	s := NewServer()
	adminOnlyHrefs := []string{"/ui/swarms", "/ui/workflows", "/ui/audit", "/ui/mcp"}
	openHrefs := []string{"/ui/tasks", "/ui/projects", "/ui/spend"}

	render := func(isAdmin bool) string {
		var buf bytes.Buffer
		if err := s.templates.ExecuteTemplate(&buf, "nav",
			struct {
				IsAdmin     bool
				CurrentPage string
			}{IsAdmin: isAdmin, CurrentPage: "dashboard"}); err != nil {
			t.Fatalf("ExecuteTemplate(nav): %v", err)
		}
		return buf.String()
	}

	nonAdmin := render(false)
	for _, h := range adminOnlyHrefs {
		want := `href="` + h + `" data-admin-link class="hidden panel-item`
		if !strings.Contains(nonAdmin, want) {
			t.Errorf("non-admin nav: %s should be hidden + data-admin-link (want %q)", h, want)
		}
	}
	for _, h := range openHrefs {
		if strings.Contains(nonAdmin, `href="`+h+`" data-admin-link`) {
			t.Errorf("non-admin nav: %s must NOT be admin-gated", h)
		}
	}

	admin := render(true)
	for _, h := range adminOnlyHrefs {
		if strings.Contains(admin, `href="`+h+`" data-admin-link class="hidden`) {
			t.Errorf("admin nav: %s should be visible (not hidden)", h)
		}
	}
}

// TestHasAdminFlag_AcceptsStructPointer covers the reflection
// helper against a typical handler data struct.
func TestHasAdminFlag_AcceptsStructPointer(t *testing.T) {
	type withFlag struct {
		IsAdmin bool
	}
	type withoutFlag struct {
		Title string
	}
	cases := []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, false},
		{"struct true", withFlag{IsAdmin: true}, true},
		{"struct false", withFlag{IsAdmin: false}, false},
		{"struct ptr true", &withFlag{IsAdmin: true}, true},
		{"struct without field", withoutFlag{Title: "x"}, false},
		{"map true", map[string]any{"IsAdmin": true}, true},
		{"map false", map[string]any{"IsAdmin": false}, false},
		{"map missing key", map[string]any{"Title": "x"}, false},
		{"string", "not a struct", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasAdminFlag(tc.in); got != tc.want {
				t.Errorf("hasAdminFlag: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMemoryIndex_DoesNotTreatAllProjectAccessAsAdmin(t *testing.T) {
	s := NewServer()
	data := struct {
		Title            string
		CurrentPage      string
		IsAdmin          bool
		Projects         []struct{}
		MemoryConfigured bool
		HardeningReady   bool
		KG               struct{ Enabled bool }
		Limit            int
		LimitOptions     []int
		TotalProjects    int
	}{
		Title:            "Memory — Vornik",
		CurrentPage:      "memory",
		IsAdmin:          false,
		MemoryConfigured: true,
		HardeningReady:   true,
	}

	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "memory_index.html", data); err != nil {
		t.Fatalf("ExecuteTemplate(memory_index.html): %v", err)
	}
	rendered := buf.String()
	if !strings.Contains(rendered, `data-admin-link class="hidden rail-ico`) {
		t.Fatalf("memory index nav should keep the admin rail hidden for non-admin data; got:\n%s", rendered)
	}
	if strings.Contains(rendered, `data-admin-link class="rail-ico`) {
		t.Fatalf("memory index nav must not render a visible admin rail for non-admin data; got:\n%s", rendered)
	}
}
