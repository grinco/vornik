// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/registry"
)

// ProjectsData holds data for the projects list template.
type ProjectsData struct {
	Title       string
	CurrentPage string
	// Rows pairs each project with its pre-computed lifecycle
	// view (archive countdown, badge state). Pre-built so the
	// template stays declarative — no method calls / time math
	// inline.
	Rows []ProjectsListRow

	// Wizard drafts banner (Feature #2 Phase C). When the wizard
	// sessions repo is wired AND the current operator has any
	// uncommitted drafts, the page surfaces a "you have N
	// unfinished wizard drafts" banner with a deep-link to the
	// most-recent one. Zero/empty hides the banner.
	WizardDraftCount     int
	WizardLatestDraftID  string
	WizardLatestDraftAgo string // pre-rendered "5m ago"
	// ArchivedCount summarises how many of the Rows are
	// archived. The template renders a "N archived" footer
	// summary when > 0.
	ArchivedCount int
}

// ProjectsListRow is one card in the projects grid. Wraps the
// raw registry.Project with the pre-formatted lifecycle view
// that drives the "Archived" badge + countdown.
type ProjectsListRow struct {
	Project   *registry.Project
	Lifecycle ProjectLifecyclePanel
}

// Projects renders the projects list page.
func (s *Server) Projects(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("rendering projects page")

	var projects []*registry.Project
	if s.projectReg != nil {
		projects = s.projectReg.ListProjects()
	} else {
		s.logger.Warn().Msg("project registry is not configured for UI")
	}

	rows := make([]ProjectsListRow, 0, len(projects))
	archivedCount := 0
	for _, p := range projects {
		if p == nil {
			continue
		}
		if !api.RequestAllowsProject(r, p.ID) {
			continue
		}
		lc := buildProjectLifecyclePanel(p)
		if lc.IsArchived {
			archivedCount++
		}
		rows = append(rows, ProjectsListRow{Project: p, Lifecycle: lc})
	}
	data := ProjectsData{
		Title:         "Projects",
		CurrentPage:   "projects",
		Rows:          rows,
		ArchivedCount: archivedCount,
	}

	// Wizard drafts — best-effort, short timeout so a slow DB
	// doesn't stall the projects page.
	if s.wizardSessions != nil {
		operator := s.operatorIDForRequest(r)
		if operator != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			rows, err := s.wizardSessions.ListByOperator(ctx, operator, 50)
			cancel()
			if err == nil {
				count := 0
				var latest *wizardDraftSummary
				for _, row := range rows {
					// Skip committed AND cancelled sessions — the banner
					// counts only genuinely-unfinished drafts. (Cancelled
					// must be excluded here too, mirroring the wizard's
					// active-session cap; otherwise a cancelled draft keeps
					// nagging in the projects UI.)
					if row == nil || row.CommittedProjectID != nil || row.CancelledAt != nil {
						continue
					}
					count++
					if latest == nil || row.UpdatedAt.After(latest.UpdatedAt) {
						latest = &wizardDraftSummary{ID: row.ID, UpdatedAt: row.UpdatedAt}
					}
				}
				if count > 0 {
					data.WizardDraftCount = count
					if latest != nil {
						data.WizardLatestDraftID = latest.ID
						data.WizardLatestDraftAgo = humanAgo(latest.UpdatedAt)
					}
				}
			}
		}
	}

	s.render(w, "projects.html", data)
}

type wizardDraftSummary struct {
	ID        string
	UpdatedAt time.Time
}

// operatorIDForRequest mirrors the api package's
// requestOperatorID heuristic, with one addition: when auth is off
// the configured single-tenant fallback fills in for an absent
// principal. Without that the drafts banner suppresses itself on
// every fresh local install (the CLI doesn't send X-Operator-Id).
// Auth-enabled deployments still get the verified principal only —
// no spoofable fallback.
func (s *Server) operatorIDForRequest(r *http.Request) string {
	return api.RequestOperatorIDOrSingleTenant(r, s.singleTenantOperatorID)
}
