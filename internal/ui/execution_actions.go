// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"errors"
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

// ExecutionRetryFromStep rewinds a FAILED or CANCELLED execution to a chosen
// step and relaunches it. The rewind itself is executor.RetryFromStep — the
// same funnel the API uses — so the state snapshot has one writer. Until
// 2026-09-04 this handler built the snapshot by hand (unmarshal to a map, set
// three keys, marshal, SaveStateSnapshot) and left the execution Paused for
// ResumePaused to pick up: a second implementation of one operation, sharing
// no code with the executor's, and the only writer of executionState that did
// not go through saveExecutionState and the pause claim
// (2026-09-04-execution-pause-write-ownership-design.md §3.2). What the
// handler keeps is what is the UI's to decide: method, form, project scope,
// and the FAILED/CANCELLED precondition (the executor also accepts COMPLETED,
// which the UI does not offer).
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

	// After the scope check on purpose: a caller outside the project learns
	// "not found", never whether the executor is wired.
	if s.executor == nil {
		http.Error(w, "execution lifecycle not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.executor.RetryFromStep(ctx, execID, chosenStepID); err != nil {
		switch {
		case errors.Is(err, executor.ErrRetryStepNotInExecution):
			http.Error(w, fmt.Sprintf("step_id %q is not in this execution's completed steps or current step", chosenStepID), http.StatusBadRequest)
		case errors.Is(err, executor.ErrRetryNotTerminal), errors.Is(err, executor.ErrRetryAlreadyExecuting):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			s.logger.Error().Err(err).Str("execution_id", execID).Str("retry_step", chosenStepID).
				Msg("retry-from-step: executor refused")
			http.Error(w, "retry from step failed", http.StatusInternalServerError)
		}
		return
	}

	s.logger.Info().
		Str("execution_id", execID).
		Str("task_id", exec.TaskID).
		Str("retry_step", chosenStepID).
		Msg("retry-from-step: execution rewound and relaunched")
	http.Redirect(w, r, fmt.Sprintf("/ui/executions/%s", execID), http.StatusSeeOther)
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
