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
}
