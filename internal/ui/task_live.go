package ui

import (
	"context"
	"errors"
	"net/http"
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
	}
	if exec != nil {
		data.CompletedSteps = exec.CompletedSteps
		if exec.CurrentStepID != nil {
			data.CurrentStep = *exec.CurrentStepID
		}
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
	}
	if exec.CurrentStepID != nil {
		data.CurrentStep = *exec.CurrentStepID
	}
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
