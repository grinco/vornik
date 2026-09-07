package ui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// The rating control on /ui/tasks/<id> (LLD
// 2026-09-04-execution-ratings-design, phase 1).
//
// The verdict attaches to the task's MOST RECENT EXECUTION, not to the task.
// That is where the attribution joins live — execution_injected_skills and
// instinct_applications are keyed the same way — and a task with three
// executions could not otherwise say which one was bad.

// TaskRatingView is what the page needs to draw the control: the run being
// judged, and the caller's own verdict when they have one.
type TaskRatingView struct {
	// ExecutionID is the run the control rates. Empty means no execution yet,
	// and the control is not drawn.
	ExecutionID string
	// Verdict is the caller's own current verdict, "" when they have not rated.
	Verdict string
	// Reason is their own reason, shown back so a re-rating starts from what
	// they said rather than from blank.
	Reason string
	// RatedAt is when they first judged this run.
	RatedAt time.Time
}

// loadTaskRating fills the view for the page. Best-effort: a read failure hides
// the control rather than failing the task page, which is the same posture the
// rest of this page takes toward its optional sections.
func (s *Server) loadTaskRating(ctx context.Context, r *http.Request, exec *persistence.Execution) *TaskRatingView {
	if s.ratingRepo == nil || exec == nil {
		return nil
	}
	view := &TaskRatingView{ExecutionID: exec.ID}
	rater := s.operatorIDForRequest(r)
	if rater == "" {
		// Nothing to show back, but the control still draws: the POST will
		// refuse and say why, which is more useful than a control that is
		// silently absent on a deployment that could fix it with a key.
		return view
	}
	existing, err := s.ratingRepo.Get(ctx, exec.ID, rater)
	if err != nil {
		return view
	}
	view.Verdict = existing.Verdict
	view.Reason = existing.Reason
	view.RatedAt = existing.CreatedAt
	return view
}

// TaskRate handles POST /tasks/{id}/rate — the page's form target.
//
// Records, replaces or withdraws the CALLER's verdict on the task's most recent
// execution, then redirects back to the page. Redirect rather than a rendered
// response so a refresh does not re-post (POST/redirect/GET).
func (s *Server) TaskRate(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.ratingRepo == nil {
		http.Error(w, "execution ratings are not wired on this deployment", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Scope first: a key scoped to project A must not rate project B's run by
	// guessing the task id. 404 rather than 403 so existence is not leaked,
	// matching the rest of this page.
	if s.taskRepo == nil {
		http.Error(w, "task repository not available", http.StatusInternalServerError)
		return
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		http.NotFound(w, r)
		return
	}
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		http.NotFound(w, r)
		return
	}

	exec := s.latestExecutionFor(ctx, taskID)
	if exec == nil {
		http.Error(w, "this task has no execution to rate", http.StatusNotFound)
		return
	}

	// The rater is resolved, never taken from the form. A form field would let
	// anyone file a judgement under another operator's name, and a rollup would
	// then be reading forged rows.
	rater := s.operatorIDForRequest(r)
	if rater == "" {
		http.Error(w,
			"could not resolve who you are, so this rating would be anonymous — "+
				"an anonymous rating cannot be edited by its author or attributed later. "+
				"This deployment needs a per-operator API key.",
			http.StatusUnauthorized)
		return
	}

	if r.FormValue("clear") != "" {
		if err := s.ratingRepo.Delete(ctx, exec.ID, rater); err != nil {
			s.logger.Warn().Err(err).Str("execution_id", exec.ID).Msg("rating: delete failed")
			http.Error(w, "failed to withdraw the rating", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/ui/tasks/"+taskID, http.StatusSeeOther)
		return
	}

	verdict := r.FormValue("verdict")
	// Closed here as well as at the API handler and in the table's CHECK, so a
	// hand-crafted form post cannot introduce a third value either.
	if verdict != persistence.VerdictUp && verdict != persistence.VerdictDown {
		http.Error(w, `verdict must be "up" or "down"`, http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) > persistence.ExecutionRatingReasonMax {
		http.Error(w,
			"the reason is longer than 500 characters; it is refused rather than "+
				"truncated, so shorten it rather than have it cut",
			http.StatusBadRequest)
		return
	}

	if err := s.ratingRepo.Upsert(ctx, &persistence.ExecutionRating{
		ExecutionID: exec.ID,
		RaterID:     rater,
		Verdict:     verdict,
		Reason:      reason,
	}); err != nil {
		s.logger.Warn().Err(err).Str("execution_id", exec.ID).Msg("rating: upsert failed")
		http.Error(w, "failed to record the rating", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/tasks/"+taskID, http.StatusSeeOther)
}

// latestExecutionFor returns the run the control rates — the most recent
// execution of the task, which is what the page shows.
func (s *Server) latestExecutionFor(ctx context.Context, taskID string) *persistence.Execution {
	if s.execRepo == nil {
		return nil
	}
	// The same query the page itself uses (newest first), so the control cannot
	// rate a different run from the one being displayed.
	execs, err := s.execRepo.List(ctx, persistence.ExecutionFilter{
		TaskID:   &taskID,
		PageSize: 1,
	})
	if err != nil || len(execs) == 0 {
		return nil
	}
	return execs[0]
}
