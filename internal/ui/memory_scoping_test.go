package ui

import (
	"strings"
	"testing"
)

// regression: 2026-06-22 — the Memory landing page showed the
// knowledge-graph extraction widget (instance-wide entity/edge/chunk
// totals; Stats() has no project filter) to project-scoped users. The
// panel is now gated to all-access (admin) callers via IsAdmin.
func TestMemoryIndex_KGPanelAdminOnly(t *testing.T) {
	s := NewServer()

	render := func(isAdmin bool) string {
		kg := map[string]any{
			"Enabled": true, "PercentDone": 42.0, "ChunksDone": 10,
			"ChunksTotal": 24, "ChunksPending": 14, "Entities": 5,
			"Edges": 3, "Mentions": 7, "EntitiesByType": []any{},
		}
		data := map[string]any{
			"Title": "Memory", "CurrentPage": "memory", "IsAdmin": isAdmin,
			"Projects": []any{}, "Enabled": false, "KG": kg,
			"Limit": 20, "LimitOptions": []int{20, 50}, "TotalProjects": 0,
		}
		var b strings.Builder
		if err := s.templates.ExecuteTemplate(&b, "memory_index.html", data); err != nil {
			t.Fatalf("render(admin=%v): %v", isAdmin, err)
		}
		return b.String()
	}

	nonAdmin := render(false)
	if strings.Contains(nonAdmin, "Knowledge graph extraction") {
		t.Error("non-admin must NOT see the instance-wide KG extraction widget")
	}
	// Operator profiles (admin-only) card hidden for non-admins.
	if !strings.Contains(nonAdmin, `data-admin-link class="hidden `) {
		t.Error("non-admin: Operator-profiles card should be hidden")
	}

	admin := render(true)
	if !strings.Contains(admin, "Knowledge graph extraction") {
		t.Error("admin should see the KG extraction widget")
	}
	if strings.Contains(admin, `data-admin-link class="hidden `) {
		t.Error("admin: Operator-profiles card should be visible (not hidden)")
	}
}
