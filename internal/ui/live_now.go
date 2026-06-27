package ui

import (
	"context"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// liveNowCard is one execution tile on the "Now Running" fleet view.
type liveNowCard struct {
	ExecutionID string
	TaskID      string
	ProjectID   string
	Status      string
	CurrentStep string
	Elapsed     string // human "Started Xm ago"
	UpdatedAgo  string // human "last event Xs ago" (heartbeat proxy)
	Stale       bool   // updated_at older than the staleness threshold

	updatedAt time.Time // sort key (unexported; templates don't read it)
}

// LiveNowData backs templates/live_now.html. Only Title + CurrentPage are
// read by the shared nav/head templates; the rest is page-local.
type LiveNowData struct {
	Title       string
	CurrentPage string // "live" → Orchestration nav area
	Cards       []liveNowCard
	Count       int
	// RefreshSeconds drives the slow re-seed backstop (F2 layers the live
	// SSE feed on top; until then this poll keeps the grid fresh).
	RefreshSeconds int
}

// liveNowStaleAfter — an execution whose updated_at is older than this is
// flagged stale (the heartbeat on the per-execution live page colours the
// same way). A live execution checkpoints far more often than this.
const liveNowStaleAfter = 90 * time.Second

// LiveNow renders the fleet "Now Running" view: every non-terminal execution
// the caller may see, as a grid of tiles linking to the per-execution live
// page. The front door to live monitoring (previously reachable only by
// drilling into a task or pasting a URL). Project-scoped exactly like the
// executions list.
func (s *Server) LiveNow(w http.ResponseWriter, r *http.Request) {
	data := LiveNowData{
		Title:          "Live",
		CurrentPage:    "live",
		RefreshSeconds: 5,
	}

	now := time.Now()
	if s.execRepo != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// Scope the DB query itself so a scoped caller's own executions
		// aren't buried past the global latest-N cap on a busy instance
		// (ui-cross-project-visibility audit). nil = all-access (single
		// global query); else per-project merge.
		queryIDs := scopeQueryIDs(r)
		// Execution non-terminal statuses. WAITING_FOR_CHILDREN is a TASK
		// state, not an execution one, so the live execution set is
		// PENDING/RUNNING/PAUSED.
		for _, st := range []persistence.ExecutionStatus{
			persistence.ExecutionStatusRunning,
			persistence.ExecutionStatusPaused,
			persistence.ExecutionStatusPending,
		} {
			stCopy := st
			execs := s.listExecutionsForScope(ctx, queryIDs, persistence.ExecutionFilter{Status: &stCopy, PageSize: 200})
			for _, e := range execs {
				if e == nil {
					continue
				}
				// Project scope — same gate as the executions list.
				if !api.RequestAllowsProject(r, e.ProjectID) {
					continue
				}
				step := ""
				if e.CurrentStepID != nil {
					step = *e.CurrentStepID
				}
				data.Cards = append(data.Cards, liveNowCard{
					ExecutionID: e.ID,
					TaskID:      e.TaskID,
					ProjectID:   e.ProjectID,
					Status:      string(e.Status),
					CurrentStep: step,
					Elapsed:     "started " + humanizeSince(now.Sub(e.CreatedAt)) + " ago",
					UpdatedAgo:  humanizeSince(now.Sub(e.UpdatedAt)) + " ago",
					Stale:       now.Sub(e.UpdatedAt) > liveNowStaleAfter,
					updatedAt:   e.UpdatedAt,
				})
			}
		}
	}

	// Newest activity first so the operator's eye lands on what just moved.
	sort.SliceStable(data.Cards, func(i, j int) bool {
		return data.Cards[i].updatedAt.After(data.Cards[j].updatedAt)
	})
	data.Count = len(data.Cards)

	s.render(w, "live_now.html", data)
}

// humanizeSince renders a duration as a compact "3m", "12s", "1h4m" string.
func humanizeSince(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String() // e.g. "12s"
	}
	if d < time.Hour {
		return d.Truncate(time.Second).String() // e.g. "3m4s"
	}
	return d.Truncate(time.Minute).String() // e.g. "1h4m0s" → fine
}
