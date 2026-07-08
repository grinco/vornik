package ui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// activePanelHref returns the href of the panel nav item rendered with the
// active-state class, or "" if none. The nav partial marks exactly one panel
// item active via `eq $.CurrentPage .Key`, so this pins which item lights up.
func activePanelHref(t *testing.T, body string) string {
	t.Helper()
	// Anchors render as: <a href="..." ...class="...panel-item panel-item-active"...>
	re := regexp.MustCompile(`<a href="([^"]+)"[^>]*\bpanel-item-active\b`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestNavHighlight_AdminSubpagesHighlightOwnItem is the regression test for the
// 2026-07-08 nav highlight bug: /ui/admin/skills and /ui/admin/keys passed the
// generic CurrentPage "admin", so the nav highlighted "Admin console" instead
// of the actual page (or nothing). Each admin surface with a dedicated panel
// item must now light up its own item.
func TestNavHighlight_AdminSubpagesHighlightOwnItem(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"alpha": {ID: "alpha"}})

	cases := []struct {
		name     string
		path     string
		wantHref string
	}{
		{"keys", "/admin/keys", "/ui/admin/keys"},
		{"skills", "/admin/skills", "/ui/admin/skills"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(WithProjectRegistry(reg))
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			s.adminRouter(rec, withAdminUI(req))
			if rec.Code != http.StatusOK {
				t.Fatalf("status: want 200, got %d", rec.Code)
			}
			if got := activePanelHref(t, rec.Body.String()); got != tc.wantHref {
				t.Errorf("active panel item href = %q, want %q (nav highlighted the wrong item)", got, tc.wantHref)
			}
		})
	}
}
