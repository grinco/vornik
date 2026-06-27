package api

import (
	"context"

	"vornik.io/vornik/internal/executor"
)

// mockPauseResumeExecutor is a mock executor for testing Pause/Resume handlers.
type mockPauseResumeExecutor struct {
	pauseStatus  *executor.PauseStatus
	resumeStatus *executor.ResumeStatus
	pauseErr     error
	resumeErr    error
	cancelErr    error
	retryErr     error
	// retryCalls captures the (executionID, stepID) tuple of every
	// RetryFromStep invocation so handler tests can assert on what
	// got dispatched without hitting a real executor.
	retryCalls []retryCall
}

type retryCall struct {
	executionID string
	stepID      string
}

func (m *mockPauseResumeExecutor) Cancel(taskID string) error {
	return m.cancelErr
}

func (m *mockPauseResumeExecutor) Pause(taskID string) (*executor.PauseStatus, error) {
	if m.pauseErr != nil {
		return nil, m.pauseErr
	}
	return m.pauseStatus, nil
}

func (m *mockPauseResumeExecutor) Resume(taskID string) (*executor.ResumeStatus, error) {
	if m.resumeErr != nil {
		return nil, m.resumeErr
	}
	return m.resumeStatus, nil
}

func (m *mockPauseResumeExecutor) RetryFromStep(_ context.Context, executionID, stepID string) error {
	m.retryCalls = append(m.retryCalls, retryCall{executionID, stepID})
	return m.retryErr
}

// NotifyChildTerminal is a no-op here; cancelNotifySpy
// (cancel_task_parent_unblock_test.go) records calls for the tests
// that assert on the parent-unblock notification.
func (m *mockPauseResumeExecutor) NotifyChildTerminal(_ context.Context, _ string) {}

var _ ExecutorInterface = (*mockPauseResumeExecutor)(nil)
