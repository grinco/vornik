package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// Cmd+K command palette. Single search-and-action surface
// opened via Cmd+K (macOS) / Ctrl+K (others) on any page.
//
// Two query types blend into one result list:
//   - Tasks (matched by short ID, full ID, or last 4 chars)
//   - Projects (matched by ID prefix)
// Plus three static "actions" that always rank near the top:
//   - "Open inbox" → /ui/tasks?status=AWAITING_INPUT
//   - "All tasks"  → /ui/tasks
//   - "Memory"     → /ui/memory
//
// Endpoint: GET /ui/palette/search?q=<query>
// Response: JSON array of result objects
//   [{kind, label, sublabel, url}]

type paletteResult struct {
	Kind     string `json:"kind"`     // "task" | "project" | "action"
	Label    string `json:"label"`    // primary line — what the operator sees
	Sublabel string `json:"sublabel"` // secondary line — short ID, project, status, etc.
	URL      string `json:"url"`      // href the palette navigates to on Enter
}

// PaletteSearch handles GET /palette/search?q=...
//
// Search shape:
//   - q=""            → just the action shortcuts
//   - q matches a known short-ID pattern (T-xxxx) → exact-match tasks
//   - q is otherwise treated as a substring across short IDs +
//     project IDs + the synthetic action labels
//
// Cap at 12 total results so the palette stays scannable. Tasks
// scoped to whatever the operator has visibility into (just the
// most recent 200 across all projects — short window keeps the
// query cheap).
func (s *Server) PaletteSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	qLower := strings.ToLower(q)

	results := make([]paletteResult, 0, 16)

	// Static actions — always present, filtered by query.
	actions := []paletteResult{
		{Kind: "action", Label: "Open inbox", Sublabel: "Tasks awaiting your input", URL: "/ui/tasks?status=AWAITING_INPUT"},
		{Kind: "action", Label: "All tasks", Sublabel: "Browse every task", URL: "/ui/tasks"},
		{Kind: "action", Label: "Memory", Sublabel: "Per-project memory health", URL: "/ui/memory"},
		{Kind: "action", Label: "Audit", Sublabel: "Tool audit log + webhook events", URL: "/ui/audit"},
		{Kind: "action", Label: "Spend", Sublabel: "LLM cost breakdowns", URL: "/ui/spend"},
		{Kind: "action", Label: "Projects", Sublabel: "All projects", URL: "/ui/projects"},
	}
	for _, a := range actions {
		if q == "" || strings.Contains(strings.ToLower(a.Label), qLower) ||
			strings.Contains(strings.ToLower(a.Sublabel), qLower) {
			results = append(results, a)
		}
	}

	// Project hits — registry lookup is in-memory, fast.
	if s.projectReg != nil && qLower != "" {
		for _, p := range s.projectReg.ListProjects() {
			if p == nil || !api.RequestAllowsProject(r, p.ID) {
				continue
			}
			if strings.Contains(strings.ToLower(p.ID), qLower) ||
				strings.Contains(strings.ToLower(p.DisplayName), qLower) {
				label := p.DisplayName
				if label == "" {
					label = p.ID
				}
				results = append(results, paletteResult{
					Kind:     "project",
					Label:    label,
					Sublabel: p.ID,
					URL:      "/ui/projects/" + p.ID,
				})
			}
			if len(results) >= 12 {
				break
			}
		}
	}

	// Task hits — short-ID prefix match (T-xxxx) or substring on
	// the full long ID. Pull the most recent 200 across all
	// projects; the substring scan is in-memory.
	if s.taskRepo != nil && qLower != "" && len(results) < 12 {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		tasks, err := s.taskRepo.List(ctx, persistence.TaskFilter{PageSize: 200})
		if err == nil {
			needle := strings.TrimPrefix(qLower, "t-")
			for _, t := range tasks {
				if len(results) >= 12 {
					break
				}
				if t == nil || !api.RequestAllowsProject(r, t.ProjectID) {
					continue
				}
				if !taskMatchesPalette(t, qLower, needle) {
					continue
				}
				short := shortID(t.ID)
				results = append(results, paletteResult{
					Kind:     "task",
					Label:    short + "  " + string(t.Status),
					Sublabel: t.ProjectID + " · " + t.ID,
					URL:      "/ui/tasks/" + t.ID,
				})
			}
		}
	}

	if len(results) > 12 {
		results = results[:12]
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(results)
}

// taskMatchesPalette is the per-task filter for the palette search.
// Matches when the lowered query is a substring of the lowered ID,
// project, status, OR is an exact suffix match against the last 4
// hex chars (the short-ID body).
func taskMatchesPalette(t *persistence.Task, q, suffixHint string) bool {
	if t == nil {
		return false
	}
	lid := strings.ToLower(t.ID)
	if strings.Contains(lid, q) {
		return true
	}
	if strings.Contains(strings.ToLower(t.ProjectID), q) {
		return true
	}
	if strings.Contains(strings.ToLower(string(t.Status)), q) {
		return true
	}
	if suffixHint != "" && len(lid) >= 4 && strings.HasSuffix(lid, suffixHint) {
		return true
	}
	return false
}
