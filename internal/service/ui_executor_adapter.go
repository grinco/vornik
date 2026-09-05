package service

// Adapter that fits *executor.Executor into ui.ExecutorInterface.
// The two interfaces drift slightly: Pause returns
// (*executor.PauseStatus, error) on the executor side but the UI
// only needs the error — the status struct carries an execution
// id and a wall-clock timestamp the UI doesn't render. Discarding
// the value here keeps the UI's interface free of an
// internal/executor import.

import (
	"context"

	"vornik.io/vornik/internal/executor"
)

// uiExecutorAdapter implements ui.ExecutorInterface over the
// concrete *executor.Executor. Wire-site only — no behaviour
// beyond signature adaptation.
type uiExecutorAdapter struct {
	e *executor.Executor
}

func (a uiExecutorAdapter) Cancel(taskID string) error {
	return a.e.Cancel(taskID)
}

// Pause discards the *PauseStatus return so the UI's narrower
// interface signature compiles. The error is what matters: nil
// = paused successfully (or task wasn't RUNNING and the caller
// will fall through to the bare TransitionConditional); non-nil
// = container stop / DB write failed.
func (a uiExecutorAdapter) Pause(taskID string) error {
	_, err := a.e.Pause(taskID)
	return err
}

func (a uiExecutorAdapter) ResumePaused(execID string) error {
	return a.e.ResumePaused(execID)
}

// RetryFromStep forwards the UI's retry-from-step action to the executor's
// rewind — the ONE writer of the rewound state since 2026-09-04
// (2026-09-04-execution-pause-write-ownership-design.md §3.2).
func (a uiExecutorAdapter) RetryFromStep(ctx context.Context, executionID, stepID string) error {
	return a.e.RetryFromStep(ctx, executionID, stepID)
}

// ResumeTask is the task-driven inverse of Pause — added 2026-05-26
// to fix the operator-observed "Resume creates a new execution
// while the paused one sits parked" bug. The UI's resume handler
// calls this first, falling back to a fresh dispatch only when no
// resumable execution exists.
func (a uiExecutorAdapter) ResumeTask(taskID string) error {
	return a.e.ResumeTask(taskID)
}

func (a uiExecutorAdapter) NotifyChildTerminal(ctx context.Context, childTaskID string) {
	a.e.NotifyChildTerminal(ctx, childTaskID)
}

// CancelIfActive forwards to the executor's live-map teardown — the
// authoritative answer to whether a cancelled task still has a container to
// stop (05-scheduler.md §4.7).
func (a uiExecutorAdapter) CancelIfActive(taskID string) (bool, error) {
	return a.e.CancelIfActive(taskID)
}

func (a uiExecutorAdapter) CancelChildren(ctx context.Context, parentTaskID string) {
	a.e.CancelChildren(ctx, parentTaskID)
}

// TaskLogs surfaces container logs for the live SSE log panel on
// /ui/tasks/{id}. Without this passthrough the UI's
// ui.WithExecutor type-assertion to TaskLogSource fails, the
// stream returns the "No logs available yet." fallback for every
// task, and the operator sees nothing despite the agent
// container actively emitting lines. (The API surface bypasses
// this adapter and calls c.Executor.TaskLogs directly, which is
// why /api/v1/projects/{p}/tasks/{id}/logs returned data while
// /ui/tasks/{id}/logs/stream returned empty — diagnosed
// 2026-05-23 on task_20260523231012_29a7e7fa321e6ea3.)
func (a uiExecutorAdapter) TaskLogs(ctx context.Context, taskID string, tail int) (string, error) {
	return a.e.TaskLogs(ctx, taskID, tail)
}
