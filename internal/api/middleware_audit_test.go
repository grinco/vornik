package api

import "testing"

// TestIsCompanionAllowedPath guards the companion-key confinement allowlist.
// A DB-backed companion (client_kind) key may use ONLY these exact paths;
// AuthMiddleware 403s it everywhere else. The match must be EXACT — never a
// prefix — so a companion key cannot reach /api/v1/internal/*,
// /api/v1/instincts/*, or /api/v1/memory/* via a trailing-segment trick.
// (Audit 2026-06-03: companion keys reached non-project surfaces because the
// old check lived only in ProjectAuthMiddleware, which skips empty-project routes.)
func TestIsCompanionAllowedPath(t *testing.T) {
	allowed := []string{"/api/v1/mcp/companion", "/api/v1/capabilities"}
	for _, p := range allowed {
		if !isCompanionAllowedPath(p) {
			t.Errorf("isCompanionAllowedPath(%q) = false, want true (legit companion surface)", p)
		}
	}

	denied := []string{
		"",
		"/api/v1/mcp/companion/",  // trailing slash is not the exact route
		"/api/v1/mcp/companion/x", // prefix trick
		"/api/v1/capabilities/x",  // prefix trick
		"/api/v1/instincts",
		"/api/v1/instincts/abc",
		"/api/v1/internal/llm-usage",
		"/api/v1/memory/reclassify-llm",
		"/api/v1/projects/foo/tasks",
		"/a2a/v1/agents/foo",
	}
	for _, p := range denied {
		if isCompanionAllowedPath(p) {
			t.Errorf("isCompanionAllowedPath(%q) = true, want false (must be confined)", p)
		}
	}
}
