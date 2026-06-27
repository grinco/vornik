// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

func (s *Server) ExecutionCancel(w http.ResponseWriter, r *http.Request, execID string) {
	if s.execRepo == nil || s.taskRepo == nil {
		http.Error(w, "execution lifecycle not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	exec, err := s.execRepo.Get(ctx, execID)
	if err != nil || exec == nil {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}
	// Scope check — a scoped key for project A must not cancel
	// project B's execution. 404 to avoid existence leak.
	if exec.ProjectID != "" && !api.RequestAllowsProject(r, exec.ProjectID) {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}

	s.cancelExecutionOne(ctx, r, execID)
	http.Redirect(w, r, fmt.Sprintf("/ui/executions/%s", execID), http.StatusSeeOther)
}

// cancelExecutionOne cancels a single execution (load → scope-check →
// executor cancel if running → mark task + execution CANCELLED). Returns
// false (skipped) when the execution is missing, scope-invisible, or the
// repos aren't wired. The shared core behind ExecutionCancel and
// ExecutionBulkCancel — mirrors cancelOne for tasks. Track C bulk-cancel.
func (s *Server) cancelExecutionOne(ctx context.Context, r *http.Request, execID string) bool {
	if s.execRepo == nil || s.taskRepo == nil {
		return false
	}
	exec, err := s.execRepo.Get(ctx, execID)
	if err != nil || exec == nil {
		return false
	}
	if exec.ProjectID != "" && !api.RequestAllowsProject(r, exec.ProjectID) {
		return false
	}
	if exec.Status == persistence.ExecutionStatusRunning && s.executor != nil {
		_ = s.executor.Cancel(exec.TaskID)
	}
	_ = s.taskRepo.UpdateStatus(ctx, exec.TaskID, persistence.TaskStatusCancelled)
	_ = s.execRepo.UpdateStatus(ctx, execID, persistence.ExecutionStatusCancelled)
	s.logger.Info().Str("execution_id", execID).Str("task_id", exec.TaskID).Msg("execution cancelled via UI")
	return true
}

// ExecutionBulkCancel handles POST /ui/executions-bulk/cancel with form field
// exec_ids (one or more). Best-effort: per-ID failures are skipped, the success
// count is reported via ?notice=bulk-exec-cancelled&count=N. Mirrors
// TaskBulkCancel. (retry-from-step is not bulkable — it needs a per-execution
// step argument — so cancel is the only bulk execution action.)
func (s *Server) ExecutionBulkCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if s.execRepo == nil || s.taskRepo == nil {
		http.Error(w, "execution lifecycle not configured", http.StatusServiceUnavailable)
		return
	}
	ids := r.Form["exec_ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/ui/executions", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cancelled := 0
	for _, id := range ids {
		if s.cancelExecutionOne(ctx, r, id) {
			cancelled++
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/executions?notice=bulk-exec-cancelled&count=%d", cancelled), http.StatusSeeOther)
}

// ExecutionStatusPartial renders just the status badge for HTMX polling.
func (s *Server) ExecutionStatusPartial(w http.ResponseWriter, r *http.Request, execID string) {
	if s.execRepo == nil {
		http.Error(w, "execution repository not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	exec, err := s.execRepo.Get(ctx, execID)
	if err != nil || exec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Status pill polls every few seconds — IDOR here would leak
	// transition observations.
	if exec.ProjectID != "" && !api.RequestAllowsProject(r, exec.ProjectID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	badge := executionStatusBadge(exec.Status)
	step := ""
	if exec.CurrentStepID != nil {
		step = *exec.CurrentStepID
	}
	// Step IDs flow from operator-authored workflow YAML, but defense in
	// depth is cheap: HTML-escape before embedding into the response. Status
	// is a typed enum and safe as-is.
	_, _ = fmt.Fprintf(w, `<span class="%s">%s</span>`, badge, exec.Status)
	if step != "" && exec.Status == persistence.ExecutionStatusRunning {
		_, _ = fmt.Fprintf(w, ` <span class="text-sm text-gray-400 ml-2">step: %s</span>`, html.EscapeString(step))
	}
	if exec.Status != persistence.ExecutionStatusRunning && exec.Status != persistence.ExecutionStatusPending {
		// Stop polling when execution reaches a terminal state.
		w.Header().Set("HX-Trigger", "stopPolling")
	}
}

// ExecutionRetryFromStep handles POST /executions/{id}/retry-from-step.
// The 2026.6.0 retry-from-step surface: operator picks a step from
// the execution's completed-steps list (or the failed step itself);
// the handler rewinds state.CurrentStepID + state.CompletedSteps to
// that step, marks every downstream outcome row as `superseded`
// (preserves audit, hides from quality rollups), parks the
// execution as Paused with PauseReasonRetryFromStep, flips the
// task off its terminal state, and kicks executor.ResumePaused so
// the resume happens in-process without waiting for the next
// daemon-restart Recover() pass.
//
// Validation refusals:
//   - execution not in {FAILED, CANCELLED} → 400
//   - step_id not in execution.CompletedSteps and not the failed
//     step → 400
//   - missing form field → 400
//   - load / persist errors → 500 with a structured error class
//
// Renders the redirect back to the execution detail page on
// success so HTMX's hx-post can swap the page and surface the new
// status.
func (s *Server) ExecutionRetryFromStep(w http.ResponseWriter, r *http.Request, execID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.execRepo == nil || s.taskRepo == nil {
		http.Error(w, "execution lifecycle not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	chosenStepID := strings.TrimSpace(r.FormValue("step_id"))
	if chosenStepID == "" {
		http.Error(w, "step_id form field required", http.StatusBadRequest)
		return
	}

	exec, err := s.execRepo.Get(ctx, execID)
	if err != nil || exec == nil {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}
	// Scope check before any state mutation — retry-from-step
	// rewinds + resumes; a cross-project mutation would be a
	// catastrophic IDOR.
	if exec.ProjectID != "" && !api.RequestAllowsProject(r, exec.ProjectID) {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}
	if exec.Status != persistence.ExecutionStatusFailed &&
		exec.Status != persistence.ExecutionStatusCancelled {
		http.Error(w, fmt.Sprintf("execution status is %s — retry only allowed on FAILED or CANCELLED", exec.Status), http.StatusBadRequest)
		return
	}

	// Validate the chosen step is reachable. Allowed positions:
	//   - any step already in CompletedSteps (re-run from there)
	//   - the failed step itself (re-attempt the step that broke)
	truncatedIdx := -1
	for i, sid := range exec.CompletedSteps {
		if sid == chosenStepID {
			truncatedIdx = i
			break
		}
	}
	failedStepMatch := exec.CurrentStepID != nil && *exec.CurrentStepID == chosenStepID
	if truncatedIdx < 0 && !failedStepMatch {
		http.Error(w, fmt.Sprintf("step_id %q is not in this execution's completed steps or current step", chosenStepID), http.StatusBadRequest)
		return
	}

	// Side-effect containment guard (mirrors executor.RetryFromStep).
	// Steps preserved upstream of the chosen step are treated as done;
	// any that produced EXTERNAL side effects (system handlers, call_project)
	// will NOT be replayed. Warn so the operator knows those effects already
	// happened and may now be stale. Best-effort: never blocks the rewind.
	if seSteps := s.sideEffectingUpstreamSteps(exec, chosenStepID); len(seSteps) > 0 {
		s.logger.Warn().
			Str("execution_id", execID).
			Str("task_id", exec.TaskID).
			Str("retry_step", chosenStepID).
			Strs("side_effecting_upstream_steps", seSteps).
			Msg("retry-from-step: preserved upstream steps had external side effects (system/call_project) that will NOT be replayed — their effects already happened and may now be stale")
	}

	// Find the recorded_at cutoff for SupersedeAfter — we
	// supersede everything STRICTLY AFTER the chosen step's
	// outcome row, so that step's row stays intact (the retry
	// produces a fresh row alongside it, and dashboards see both
	// the original outcome and the retry's verdict).
	var cutoff time.Time
	if s.outcomeRepo != nil {
		outcomes, lerr := s.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &execID})
		if lerr == nil {
			for _, o := range outcomes {
				if o == nil || o.StepID != chosenStepID {
					continue
				}
				if o.RecordedAt.After(cutoff) {
					cutoff = o.RecordedAt
				}
			}
		} else {
			s.logger.Warn().Err(lerr).Str("execution_id", execID).
				Msg("retry-from-step: outcome list failed; supersede cutoff defaults to NOW")
		}
	}
	if cutoff.IsZero() {
		// No outcome row matched (gate step, or pre-outcome-table
		// era). Use NOW so SupersedeAfter is a no-op for downstream
		// rows that don't exist yet — the new retry's rows will all
		// have later recorded_at and stay live.
		cutoff = time.Now().UTC()
	}

	// Persist supersede + rewound state in dependency order.
	// SupersedeAfter is idempotent (already-superseded rows stay
	// superseded), so a retry of this handler after a partial
	// failure is safe.
	if s.outcomeRepo != nil {
		if _, sup := s.outcomeRepo.SupersedeAfter(ctx, execID, cutoff); sup != nil {
			s.logger.Warn().Err(sup).Str("execution_id", execID).
				Msg("retry-from-step: SupersedeAfter failed; outcomes may double-count")
		}
	}

	// Rewind the in-memory state to the chosen step. The new
	// CompletedSteps slice keeps everything up to (but not
	// including) the chosen step; the chosen step itself becomes
	// the next CurrentStepID so the resumed loop re-runs it.
	newCompleted := append([]string{}, exec.CompletedSteps...)
	if truncatedIdx >= 0 {
		newCompleted = newCompleted[:truncatedIdx]
	} // else: failed-step path — keep all completed, just re-run the failed step.

	// Load the existing state snapshot so retry preserves the
	// arbitrary executor fields (iteration count, visit counts,
	// last result) that the workflow loop wants. Mutate only the
	// retry-relevant fields.
	var state map[string]any
	if len(exec.StateSnapshot) > 0 {
		_ = json.Unmarshal(exec.StateSnapshot, &state)
	}
	if state == nil {
		state = map[string]any{}
	}
	state["currentStepId"] = chosenStepID
	state["completedSteps"] = newCompleted
	state["pausedReason"] = executor.PauseReasonRetryFromStep
	snapshot, mErr := json.Marshal(state)
	if mErr != nil {
		s.logger.Error().Err(mErr).Str("execution_id", execID).
			Msg("retry-from-step: state marshal failed")
		http.Error(w, "internal state marshal failed", http.StatusInternalServerError)
		return
	}
	if err := s.execRepo.SaveStateSnapshot(ctx, execID, snapshot, chosenStepID, newCompleted); err != nil {
		s.logger.Error().Err(err).Str("execution_id", execID).
			Msg("retry-from-step: save state failed")
		http.Error(w, "save state failed", http.StatusInternalServerError)
		return
	}
	if err := s.execRepo.UpdateStatus(ctx, execID, persistence.ExecutionStatusPaused); err != nil {
		s.logger.Error().Err(err).Str("execution_id", execID).
			Msg("retry-from-step: flip to paused failed")
		http.Error(w, "flip to paused failed", http.StatusInternalServerError)
		return
	}
	// Re-arm the task: was FAILED / CANCELLED, must be non-terminal
	// for recoverExecution's check to let the resume through.
	// RUNNING matches what the resume path expects.
	if err := s.taskRepo.UpdateStatus(ctx, exec.TaskID, persistence.TaskStatusRunning); err != nil {
		s.logger.Error().Err(err).Str("task_id", exec.TaskID).
			Msg("retry-from-step: task UpdateStatus failed")
		http.Error(w, "task re-arm failed", http.StatusInternalServerError)
		return
	}

	// Trigger the in-process resume so the operator sees the
	// execution move immediately instead of having to wait for a
	// daemon restart's Recover() pass. ResumePaused is best-effort —
	// a failure here just leaves the row Paused, which Recover()
	// would still pick up.
	if s.executor != nil {
		if rerr := s.executor.ResumePaused(execID); rerr != nil {
			s.logger.Warn().Err(rerr).Str("execution_id", execID).
				Msg("retry-from-step: in-process ResumePaused failed; row is Paused, daemon Recover() will retry on next start")
		}
	}

	s.logger.Info().
		Str("execution_id", execID).
		Str("task_id", exec.TaskID).
		Str("retry_step", chosenStepID).
		Time("supersede_cutoff", cutoff).
		Int("completed_after_rewind", len(newCompleted)).
		Msg("retry-from-step: execution rewound and resumed")
	http.Redirect(w, r, fmt.Sprintf("/ui/executions/%s", execID), http.StatusSeeOther)
}

// sideEffectingUpstreamSteps returns the preserved-upstream step IDs whose
// workflow step type produces external side effects a retry-from-step will not
// replay (registry.WorkflowStep.HasExternalSideEffects). Survivors mirror the
// rewind below: everything before chosenStepID in CompletedSteps, or — on the
// failed-step path (chosenStepID == CurrentStepID, not yet completed) — all
// completed steps. Best-effort and read-only: nil if the registry/workflow is
// unavailable, so the advisory never blocks the rewind.
func (s *Server) sideEffectingUpstreamSteps(exec *persistence.Execution, chosenStepID string) []string {
	if exec == nil || s.projectReg == nil || exec.WorkflowID == "" {
		return nil
	}
	survivors := exec.CompletedSteps
	for i, sid := range exec.CompletedSteps {
		if sid == chosenStepID {
			survivors = exec.CompletedSteps[:i]
			break
		}
	}
	if len(survivors) == 0 {
		return nil
	}
	wf := s.projectReg.GetWorkflow(exec.WorkflowID)
	if wf == nil {
		return nil
	}
	var out []string
	for _, id := range survivors {
		if step, ok := wf.Steps[id]; ok && step.HasExternalSideEffects() {
			out = append(out, id)
		}
	}
	return out
}

// executionStatusBadge returns the theme-aware semantic pill classes for an
// execution status. The .pill primitive (internal/ui/templates/_partials.html)
// owns padding/shape/contrast in both themes; callers render
// <span class="{{...}}"> with no extra utility classes.
func executionStatusBadge(status persistence.ExecutionStatus) string {
	switch status {
	case persistence.ExecutionStatusRunning:
		return "pill pill-info"
	case persistence.ExecutionStatusCompleted:
		return "pill pill-ok"
	case persistence.ExecutionStatusFailed:
		return "pill pill-danger"
	case persistence.ExecutionStatusPaused:
		return "pill pill-warn"
	default:
		return "pill pill-neutral"
	}
}
