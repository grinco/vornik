package ui

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
)

// LiveTaskPageData is the template payload for /ui/tasks/<id>/live
// and /ui/executions/<id>/live. The template renders a header from
// Task + Execution metadata; the WebSocket-driven JS layer fills the
// timeline as events arrive.
//
// CompletedSteps and CurrentStep are surfaced as separate template
// fields so the Fork modal's step picker can offer them without a
// second round-trip — both come straight off Execution but rendering
// them at the Go level keeps the template free of nil-dereference
// guards on the *string columns.
type LiveTaskPageData struct {
	Title       string
	CurrentPage string
	Task        *persistence.Task
	Execution   *persistence.Execution
	// CompletedSteps mirrors Execution.CompletedSteps but typed as a
	// plain slice so the template `range` works without unwrapping
	// the pointer-to-string columns the Execution carries elsewhere.
	CompletedSteps []string
	// CurrentStep is the dereferenced Execution.CurrentStepID. Empty
	// when the execution hasn't started its first step yet.
	CurrentStep string
	// Outcomes are the recorded per-attempt step outcomes (oldest first),
	// each carrying its OWN status — the same source of truth the non-live
	// /ui/executions/{id} page uses. Seeding the timeline from these (instead
	// of a hardcoded "completed"/"running" badge per step id) is the fix for
	// the 2026-07-08 report where every retry attempt showed RUNNING: a failed
	// attempt now renders "failed", only the live attempt is "running".
	Outcomes []StepOutcomeRow
	// StoryLines seeds the plain-language "story" panel from the
	// execution_narration store (task 2.2, narrated-execution-design.md
	// §5.6), ordered by seq ascending. Empty when the narrator isn't
	// wired or the execution has no narration yet — the panel then
	// renders its empty state and fills in live over the WebSocket.
	StoryLines []StoryLineRow
	// IsRoleUser is true for a project-scoped, non-admin session
	// (api.SessionRoleFromContext == auth.RoleUser). The story panel is
	// always the default-open primary content; this flag instead
	// decides whether the technical "Step timeline" section defaults
	// open (false — admin/no-session, today's behaviour) or collapsed
	// behind "Show technical details" (true — RoleUser).
	IsRoleUser bool
}

// liveStepOutcomes loads the recorded step-outcome rows for an execution and
// projects them (oldest first) for the live-timeline seed. Mirrors the
// non-live execution-detail projection but returns only the fields the live
// timeline renders. Best-effort: a nil repo or a load error yields no seed
// rows (the JS layer still fills the live timeline from the WebSocket).
func (s *Server) liveStepOutcomes(ctx context.Context, executionID string) []StepOutcomeRow {
	if s.outcomeRepo == nil || executionID == "" {
		return nil
	}
	rows, err := s.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		ExecutionID: &executionID,
		PageSize:    200,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("execution_id", executionID).
			Msg("live: failed to load step outcomes for timeline seed")
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	// Repo returns newest first; reverse to execution order (RecordedAt asc,
	// ID tie-break) — same ordering the execution-detail page uses.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].RecordedAt.Equal(rows[j].RecordedAt) {
			return rows[i].RecordedAt.Before(rows[j].RecordedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]StepOutcomeRow, 0, len(rows))
	for _, o := range rows {
		row := StepOutcomeRow{
			StepID:       o.StepID,
			Role:         o.Role,
			Model:        o.Model,
			Outcome:      o.Outcome,
			ErrorClass:   o.ErrorClass,
			ErrorDetail:  o.ErrorDetail,
			OutcomeClass: outcomeCSSClass(o.Outcome),
		}
		out = append(out, row)
	}
	return out
}

// TaskLive renders the live observation page for /ui/tasks/<id>/live.
// Terminal-status tasks redirect to /ui/tasks/<id> (operators land on
// the static task detail / replay path post-hoc); non-terminal tasks
// render the live page with the most recent execution pre-filled.
//
// Path-prefix slicing follows the same pattern as TaskDetail; the
// /live suffix is stripped by the dispatcher before calling.
func (s *Server) TaskLive(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if taskID == "" {
		http.Error(w, "task id required", http.StatusBadRequest)
		return
	}
	if s.taskRepo == nil || s.execRepo == nil {
		http.Error(w, "live observation not available on this deployment",
			http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("live: task lookup failed")
		http.NotFound(w, r)
		return
	}
	if task == nil {
		http.NotFound(w, r)
		return
	}
	// Project-scope check — a scoped key for project A must not
	// observe project B's live stream. 404 to avoid existence
	// leak. Empty ProjectID (legacy rows) bypasses; the in-tree
	// convention is that unowned rows are admin/auth-off visible.
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		http.NotFound(w, r)
		return
	}

	// Terminal-status visits go to the replay page per the design
	// doc — once the task is finished the live stream has nothing
	// useful to say. CLOSED is the conversational-lifecycle terminal
	// for COMPLETED tasks; we treat it the same here.
	if isTerminalTaskStatus(task.Status) {
		http.Redirect(w, r, "/ui/tasks/"+taskID, http.StatusFound)
		return
	}

	// Find the most recent non-terminal execution. List() returns
	// newest first; the first non-terminal row is the one the
	// scheduler/executor is currently working on.
	taskIDCopy := taskID
	execs, err := s.execRepo.List(ctx, persistence.ExecutionFilter{
		TaskID:   &taskIDCopy,
		PageSize: 20,
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("live: execution list failed")
		http.Error(w, "failed to load executions", http.StatusInternalServerError)
		return
	}
	var exec *persistence.Execution
	for _, e := range execs {
		if e == nil {
			continue
		}
		if !isTerminalExecutionStatus(e.Status) {
			exec = e
			break
		}
	}
	// Fall back to the newest execution even if it's terminal — the
	// page header still has something to render, and the JS layer
	// will surface "closed" via the WebSocket's final frame. The
	// task-level terminal redirect above already covers the
	// genuinely-terminal case; this fallback handles the narrow
	// window where the task is still LEASED/QUEUED but no execution
	// row exists yet.
	if exec == nil && len(execs) > 0 {
		exec = execs[0]
	}

	data := LiveTaskPageData{
		Title:       "Live — " + taskID,
		CurrentPage: "tasks",
		Task:        task,
		Execution:   exec,
		IsRoleUser:  isStoryDefaultViewer(r),
	}
	if exec != nil {
		data.CompletedSteps = exec.CompletedSteps
		if exec.CurrentStepID != nil {
			data.CurrentStep = *exec.CurrentStepID
		}
		data.Outcomes = s.liveStepOutcomes(ctx, exec.ID)
		data.StoryLines = s.storyLines(ctx, exec.ID)
	}
	s.render(w, "task_live.html", data)
}

// ExecutionLive renders the live page from the execution side so
// operators can deep-link from /ui/executions/<id>. The execution's
// task is looked up to share the same template; terminal-status
// executions redirect to the replay surface (post-hoc inspection).
func (s *Server) ExecutionLive(w http.ResponseWriter, r *http.Request, execID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if execID == "" {
		http.Error(w, "execution id required", http.StatusBadRequest)
		return
	}
	if s.taskRepo == nil || s.execRepo == nil {
		http.Error(w, "live observation not available on this deployment",
			http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	exec, err := s.execRepo.Get(ctx, execID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("execution_id", execID).Msg("live: execution lookup failed")
		http.NotFound(w, r)
		return
	}
	if exec == nil {
		http.NotFound(w, r)
		return
	}
	// Multi-tenant scope gate (S1, audit 2026-07-03): a project-scoped caller
	// must not observe another tenant's execution. Mirrors ExecutionDetail.
	if !s.uiRequireProjectScope(w, r, exec.ProjectID) {
		return
	}

	// Terminal executions go to the replay page — the operator
	// lands there for post-hoc forensics rather than an empty live
	// stream.
	if isTerminalExecutionStatus(exec.Status) {
		http.Redirect(w, r, "/ui/executions/"+execID+"/replay", http.StatusFound)
		return
	}

	task, err := s.taskRepo.Get(ctx, exec.TaskID)
	if err != nil || task == nil {
		// Task missing while execution is live is a data-integrity
		// edge case (cascade delete race). Render the page with a
		// nil Task; the template guards on it.
		s.logger.Warn().Err(err).Str("task_id", exec.TaskID).Msg("live: task lookup failed for execution")
	}

	data := LiveTaskPageData{
		Title:          "Live — " + execID,
		CurrentPage:    "tasks",
		Task:           task,
		Execution:      exec,
		CompletedSteps: exec.CompletedSteps,
		IsRoleUser:     isStoryDefaultViewer(r),
	}
	if exec.CurrentStepID != nil {
		data.CurrentStep = *exec.CurrentStepID
	}
	data.Outcomes = s.liveStepOutcomes(ctx, exec.ID)
	data.StoryLines = s.storyLines(ctx, exec.ID)
	s.render(w, "task_live.html", data)
}

// isTerminalTaskStatus encodes the design-doc rule: COMPLETED,
// FAILED, CANCELLED, and CLOSED visits skip the live page and land
// on the task detail / replay surface. AWAITING_INPUT and
// AWAITING_EXTERNAL are conversational waits — not terminal, the
// task may resume — so they stay on the live page.
func isTerminalTaskStatus(s persistence.TaskStatus) bool {
	switch s {
	case persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed:
		return true
	}
	return false
}

// isTerminalExecutionStatus is the execution-side companion. Mirrors
// the executor's terminal set so a fork-spawned execution that
// completed independently of its parent task still redirects to
// replay.
func isTerminalExecutionStatus(s persistence.ExecutionStatus) bool {
	switch s {
	case persistence.ExecutionStatusCompleted,
		persistence.ExecutionStatusFailed,
		persistence.ExecutionStatusCancelled:
		return true
	}
	return false
}
