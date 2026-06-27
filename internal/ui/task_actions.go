// Code in this file was extracted from server.go to keep the
// per-page handlers grouped with their data types.

package ui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// projectRouter dispatches project detail and project config editor requests.

// cancelOne attempts to cancel a single task. Returns true if the task
// transitioned to CANCELLED, false if it was already in a terminal state
// (no-op), not found, or not visible to the caller's project scope.
// Reused by TaskCancel and TaskBulkCancel so the bulk path doesn't
// drift from the single path — including the scope check (a scoped
// key for project A must not cancel a project B task by guessing IDs).
func (s *Server) cancelOne(ctx context.Context, r *http.Request, taskID string) bool {
	if s.taskRepo == nil {
		return false
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		// Treat as not-found from the caller's perspective so
		// existence isn't leaked through the cancellable/not-found
		// branching in TaskCancel.
		return false
	}
	switch task.Status {
	case persistence.TaskStatusQueued, persistence.TaskStatusPending,
		persistence.TaskStatusLeased, persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput, persistence.TaskStatusAwaitingExternal,
		persistence.TaskStatusPaused:
		// cancellable — non-terminal per the scheduler state machine.
		// Pre-fix only the executor-driven statuses were listed, so a
		// parent stuck in WAITING_FOR_CHILDREN (operator closed the
		// child without the parent-unblock hook firing) had no UI exit
		// short of a direct DB update. AWAITING_* and PAUSED are
		// likewise non-terminal and should be operator-cancellable —
		// state_machine.TriggerOperatorCancel allows "any non-terminal
		// → CANCELLED".
	default:
		return false
	}
	if task.Status == persistence.TaskStatusRunning && s.executor != nil {
		_ = s.executor.Cancel(taskID)
	}
	_ = s.taskRepo.UpdateStatus(ctx, taskID, persistence.TaskStatusCancelled)
	// CANCELLED is terminal, so drive the parent-unblock sweep —
	// same wiring as the close path (uiCloseTask). For a non-running
	// child the executor's handleCancelled never fires, so without
	// this a WAITING_FOR_CHILDREN parent waited for the cancelled
	// child forever (regression 2026-06-07; the WAITING_FOR_CHILDREN
	// case in the switch above was treating that symptom).
	if task.ParentTaskID != nil && *task.ParentTaskID != "" && s.executor != nil {
		s.executor.NotifyChildTerminal(ctx, taskID)
	}
	s.logger.Info().Str("task_id", taskID).Msg("task cancelled via UI")
	return true
}

func (s *Server) TaskCancel(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !s.cancelOne(ctx, r, taskID) {
		// Either not found, scope-mismatch, or not cancellable.
		// For scope-mismatch we want to look indistinguishable from
		// not-found, so consult the same Get and short-circuit to
		// 404 when the row is invisible to this caller.
		t, _ := s.taskRepo.Get(ctx, taskID)
		if t == nil || (t.ProjectID != "" && !api.RequestAllowsProject(r, t.ProjectID)) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/ui/tasks/%s?notice=task-not-cancellable", taskID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks/%s?notice=task-cancelled", taskID), http.StatusSeeOther)
}

// TaskBulkCancel handles POST /ui/tasks-bulk/cancel with form field
// task_ids (one or more values). Reports a count via ?notice= on the
// redirect target so the tasks-list toast tells the user what happened.
func (s *Server) TaskBulkCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ids := r.Form["task_ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/ui/tasks", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cancelled := 0
	for _, id := range ids {
		if s.cancelOne(ctx, r, id) {
			cancelled++
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks?notice=bulk-cancelled&count=%d", cancelled), http.StatusSeeOther)
}

// TaskStatusPartial renders just the status badge for HTMX polling.
// Mirrors ExecutionStatusPartial — returns plain HTML and sets
// HX-Trigger: stopPolling once the task reaches a terminal state.
func (s *Server) TaskStatusPartial(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Scope check — the status pill polls every few seconds, so an
	// IDOR here would leak status-transition observations.
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, "taskStatusBadge", task.Status); err != nil {
		s.logger.Warn().Err(err).Msg("TaskStatusPartial: render failed")
	}
	switch task.Status {
	case persistence.TaskStatusQueued, persistence.TaskStatusPending,
		persistence.TaskStatusLeased, persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren:
		// keep polling
	default:
		w.Header().Set("HX-Trigger", "stopPolling")
	}
}

// retryOne attempts to re-queue a single task. Returns true on success,
// false if not found or not retriable. Reused by TaskRetry / TaskBulkRetry.
//
// Uses RequeueTerminalTask, which gates the WRITE on the live terminal
// status set inside the SQL — so a concurrent scheduler pickup or
// duplicate retry click cannot re-queue a task that has already left
// the terminal state. The prior implementation called ReleaseLease
// with an empty leaseID, which silently bypassed the lease identity
// check and could overwrite an in-flight execution.
func (s *Server) retryOne(ctx context.Context, r *http.Request, taskID string) bool {
	if s.taskRepo == nil {
		return false
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		return false
	}
	switch task.Status {
	case persistence.TaskStatusFailed, persistence.TaskStatusCancelled, persistence.TaskStatusCompleted:
		// retriable
	case persistence.TaskStatusPending:
		// PENDING is a stuck-waiting state, not strictly terminal,
		// but tasks land here when (a) a paused execution's resume
		// signal never arrives, or (b) a retry-from-step + scheduler
		// retry race leaves the task with no live dispatcher driving
		// it. Operator-observed 2026-05-16 on
		// task_20260516015931_2c9658cb6a103380 — after 5 of 6
		// attempts, status stayed PENDING and the scheduler's
		// lease-pickup query (WHERE status = 'QUEUED') silently
		// skipped it forever. Allow operator-initiated retry as the
		// safety net: the operator clicked, they know what they want.
		// retriable
	default:
		return false
	}
	attempt := task.Attempt + 1
	maxAttempts := task.MaxAttempts
	if maxAttempts <= attempt {
		maxAttempts = attempt + 2 // give room for retries
	}
	transitioned, err := s.taskRepo.RequeueTerminalTask(ctx, taskID, attempt, maxAttempts)
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("task retry via UI failed")
		return false
	}
	if !transitioned {
		s.logger.Info().Str("task_id", taskID).Msg("task retry via UI lost race; task no longer terminal")
		return false
	}
	s.logger.Info().Str("task_id", taskID).Int("attempt", attempt).Msg("task retried via UI")
	return true
}

// applyFallbackModelOverride rewrites a task's payload so every role
// with a configured modelFallback runs on that fallback for the next
// execution (the "retry on fallback model" operator action). Returns
// true when an override was written. Best-effort: a missing registry,
// a swarm with no fallbacks, or a persistence error logs and returns
// false so the retry still proceeds on the primary models.
func (s *Server) applyFallbackModelOverride(ctx context.Context, task *persistence.Task) bool {
	if s.projectReg == nil || s.taskRepo == nil {
		return false
	}
	applied, err := executor.ApplyFallbackModelOverride(ctx, s.projectReg, s.taskRepo, task)
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", task.ID).
			Msg("retry-on-fallback: override failed; running on primary models")
		return false
	}
	if !applied {
		s.logger.Info().Str("task_id", task.ID).
			Msg("retry-on-fallback: no role has a modelFallback configured; nothing to override")
		return false
	}
	s.logger.Info().Str("task_id", task.ID).Msg("retry-on-fallback: applied operator model override")
	return true
}

// TaskRetry re-queues a failed, cancelled, or completed task. When the
// form carries fallback_model=1 (the "Retry on fallback model" button)
// the task's roles are first switched to their configured fallback
// models for the next run.
func (s *Server) TaskRetry(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := r.ParseForm(); err == nil && r.FormValue("fallback_model") == "1" {
		if task, err := s.taskRepo.Get(ctx, taskID); err == nil && task != nil &&
			(task.ProjectID == "" || api.RequestAllowsProject(r, task.ProjectID)) {
			s.applyFallbackModelOverride(ctx, task)
		}
	}

	if !s.retryOne(ctx, r, taskID) {
		t, _ := s.taskRepo.Get(ctx, taskID)
		if t == nil || (t.ProjectID != "" && !api.RequestAllowsProject(r, t.ProjectID)) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/ui/tasks/%s?notice=task-not-retriable", taskID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks/%s?notice=task-retried", taskID), http.StatusSeeOther)
}

// TaskBulkRetry handles POST /ui/tasks-bulk/retry with form field task_ids.
func (s *Server) TaskBulkRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ids := r.Form["task_ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/ui/tasks", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	retried := 0
	for _, id := range ids {
		if s.retryOne(ctx, r, id) {
			retried++
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks?notice=bulk-retried&count=%d", retried), http.StatusSeeOther)
}

// closeOne closes a single task, mirroring cancelOne's shape (load →
// scope-check → act → bool) but delegating the actual close to uiCloseTask so
// the close semantics — transition guard, audit message, parent-unblock sweep,
// SSE publish — live in exactly one place. Returns false (skipped) when the
// task is not found, scope-invisible, or in a status uiCloseTask deems
// ineligible. Track C bulk-Close.
func (s *Server) closeOne(ctx context.Context, r *http.Request, taskID string) bool {
	if s.taskRepo == nil || s.taskMessageRepo == nil {
		return false
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	if task.ProjectID != "" && !api.RequestAllowsProject(r, task.ProjectID) {
		// Scope-invisible → treat as not-found, matching cancelOne.
		return false
	}
	return s.uiCloseTask(ctx, task, r) == "task-closed"
}

// TaskBulkClose handles POST /ui/tasks-bulk/close with form field task_ids
// (one or more values). Best-effort: per-ID ineligibility/failures are skipped,
// the success count is reported via ?notice=bulk-closed&count=N. Mirrors
// TaskBulkCancel / TaskBulkRetry.
func (s *Server) TaskBulkClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if s.taskRepo == nil {
		http.Error(w, "task repository not configured", http.StatusServiceUnavailable)
		return
	}
	ids := r.Form["task_ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/ui/tasks", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	closed := 0
	for _, id := range ids {
		if s.closeOne(ctx, r, id) {
			closed++
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/ui/tasks?notice=bulk-closed&count=%d", closed), http.StatusSeeOther)
}

// --- Execution actions ---
