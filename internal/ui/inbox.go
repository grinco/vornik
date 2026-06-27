package ui

import (
	"context"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// inboxItem is one actionable row in the "Needs you" inbox.
type inboxItem struct {
	TaskID    string
	ProjectID string
	Kind      string // human label: "Needs approval" / "Needs input" / "Failed" / "Blocked"
	Detail    string // error class for failures, etc.
	Age       string // "5m ago"
	Action    string // CTA label, e.g. "Approve / reject"
	Href      string // where the action lives (task detail)

	rank      int       // category rank for sorting (lower = more urgent)
	createdAt time.Time // within-category tiebreak (oldest first)
}

// InboxData backs templates/inbox.html.
type InboxData struct {
	Title       string
	CurrentPage string // "inbox" → Orchestration nav area
	Items       []inboxItem
	Count       int
}

// inboxCategory pairs a task status with its presentation.
type inboxCategory struct {
	status persistence.TaskStatus
	rank   int
	kind   string
	action string
}

// Inbox renders the unified "what needs me" queue: AWAITING_APPROVAL +
// AWAITING_INPUT (a human is blocked — ranked first) then recent FAILED and
// WAITING_FOR_CHILDREN (already terminal / waiting). Promotes the dashboard's
// count-only operator-queue cards to a drill-in list with per-item metadata
// and a direct link to where the action lives. Project-scoped.
func (s *Server) Inbox(w http.ResponseWriter, r *http.Request) {
	data := InboxData{Title: "Needs you", CurrentPage: "inbox"}

	if s.taskRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		cats := []inboxCategory{
			{persistence.TaskStatusAwaitingApproval, 1, "Needs approval", "Approve / reject"},
			{persistence.TaskStatusAwaitingInput, 2, "Needs input", "Answer checkpoint"},
			{persistence.TaskStatusFailed, 3, "Failed", "Review / retry"},
			{persistence.TaskStatusWaitingForChildren, 4, "Blocked on children", "Inspect"},
		}
		failedCutoff := time.Now().Add(-24 * time.Hour)
		// Scope the DB query itself: a global latest-N slice lets a busy
		// instance's other-project rows bury a scoped caller's own
		// actionable items past the cap (ui-cross-project-visibility audit).
		// nil = all-access (single global query); else per-project merge.
		queryIDs := scopeQueryIDs(r)

		for _, c := range cats {
			st := c.status
			tasks := s.listTasksForScope(ctx, queryIDs, persistence.TaskFilter{Status: &st, PageSize: 200})
			for _, t := range tasks {
				if t == nil || !api.RequestAllowsProject(r, t.ProjectID) {
					continue
				}
				// FAILED is unbounded over time; only surface the last 24h
				// (matches the dashboard's failed-recently card).
				if st == persistence.TaskStatusFailed && t.UpdatedAt.Before(failedCutoff) {
					continue
				}
				detail := ""
				if t.LastErrorClass != nil {
					detail = *t.LastErrorClass
				}
				data.Items = append(data.Items, inboxItem{
					TaskID:    t.ID,
					ProjectID: t.ProjectID,
					Kind:      c.kind,
					Detail:    detail,
					Age:       humanizeSince(time.Since(t.CreatedAt)) + " ago",
					Action:    c.action,
					Href:      "/ui/tasks/" + t.ID,
					rank:      c.rank,
					createdAt: t.CreatedAt,
				})
			}
		}
	}

	// Rank by urgency, then oldest-first within a category.
	sort.SliceStable(data.Items, func(i, j int) bool {
		if data.Items[i].rank != data.Items[j].rank {
			return data.Items[i].rank < data.Items[j].rank
		}
		return data.Items[i].createdAt.Before(data.Items[j].createdAt)
	})
	data.Count = len(data.Items)

	s.render(w, "inbox.html", data)
}
