package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// cancelNotifySpy extends the pause/resume mock with a recording
// NotifyChildTerminal so the CancelTask handler's parent-unblock
// notification can be asserted on.
type cancelNotifySpy struct {
	mockPauseResumeExecutor
	notifyCalls []string
}

func (s *cancelNotifySpy) NotifyChildTerminal(_ context.Context, childTaskID string) {
	s.notifyCalls = append(s.notifyCalls, childTaskID)
}

// TestServer_CancelTask_NotifiesParentUnblock — regression test for
// the 2026-06-07 child-cancel incident: cancelling a child task via
// the API never drove the executor's parent-unblock sweep, so a
// parent in WAITING_FOR_CHILDREN waited for the cancelled child
// forever. For a non-running child (QUEUED here) the executor's own
// handleCancelled path never fires at all, so CancelTask must call
// NotifyChildTerminal itself after the transition succeeds.
func TestServer_CancelTask_NotifiesParentUnblock(t *testing.T) {
	parentID := "parent-1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{
				ID:           "task-1",
				ProjectID:    "project-1",
				ParentTaskID: &parentID,
				Status:       persistence.TaskStatusQueued,
				CreatedAt:    time.Now(),
			}, nil
		},
		TransitionToCancelledFunc: func(_ context.Context, _ string) (bool, error) {
			return true, nil
		},
	}
	spy := &cancelNotifySpy{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(taskRepo),
		WithExecutor(spy),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"task-1"}, spy.notifyCalls,
		"CancelTask must notify the executor so the parent-unblock sweep runs")
}
