// Package api provides HTTP handlers for the vornik data plane API.
package api

import (
	"context"

	"vornik.io/vornik/internal/executor"
)

// ExecutorInterface defines the methods the API layer needs from the executor.
// This allows for easier testing without coupling to the concrete type.
type ExecutorInterface interface {
	Cancel(taskID string) error
	Pause(taskID string) (*executor.PauseStatus, error)
	Resume(taskID string) (*executor.ResumeStatus, error)
	// RetryFromStep restarts a terminal execution from the named
	// step. The API translates well-known sentinel errors
	// (executor.ErrRetry*) into 4xx responses; other errors are 500.
	RetryFromStep(ctx context.Context, executionID, stepID string) error
	// NotifyChildTerminal drives the executor's parent-unblock sweep
	// after a task reaches a terminal status outside the executor's
	// own flow (e.g. CancelTask on a non-running child, where
	// handleCancelled never fires). No-op for tasks without a parent.
	NotifyChildTerminal(ctx context.Context, childTaskID string)
	// CancelChildren recursively cancels the non-terminal in-project
	// children of a just-cancelled parent (the downward cascade), so a
	// cancelled parent doesn't leave its route/delegation/checkpoint
	// children RUNNING/QUEUED. Idempotent + race-safe; cross-project
	// callees are handled separately via the CPC ledger.
	// CancelIfActive tears down a live execution for taskID if the executor
	// is running one, reporting whether it was. This — not the task's status
	// — is what decides whether a cancelled task needs its container
	// stopped: a status read before the transition can be stale, and a task
	// that reached RUNNING in that window had its row cancelled and its
	// container left alive (05-scheduler.md §4.7). (false, nil) means
	// nothing to stop, which is routine; (true, err) means a container is
	// still running and could not be stopped.
	CancelIfActive(taskID string) (bool, error)
	CancelChildren(ctx context.Context, parentTaskID string)
}
