// Regression tests for S4 (audit 2026-07-03): the SSE task-event stream gate
// must fail CLOSED. Pre-fix it only denied when the task lookup SUCCEEDED, so a
// lookup error or a missing task fell through to a live subscription — a
// transient DB error plus a guessed foreign task id leaked another project's
// stream. A pre-cancelled request context keeps the (pre-fix) stream loop from
// blocking the test; the assertion is on the status code the gate produces.
package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func sseReq(taskID string) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: any (pre-fix) stream loop exits immediately
	r := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID+"/events", nil)
	return r.WithContext(ctx)
}

func TestTaskEventsStream_FailsClosedOnLookupError(t *testing.T) {
	s := &Server{
		sseBus: NewSSEBus(),
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return nil, errors.New("transient DB error")
			},
		},
	}
	rec := httptest.NewRecorder()
	s.TaskEventsStream(rec, sseReq("t1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("lookup error must fail closed: want 404, got %d", rec.Code)
	}
}

func TestTaskEventsStream_FailsClosedOnMissingTask(t *testing.T) {
	s := &Server{
		sseBus: NewSSEBus(),
		taskRepo: &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
				return nil, nil
			},
		},
	}
	rec := httptest.NewRecorder()
	s.TaskEventsStream(rec, sseReq("t1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing task must fail closed: want 404, got %d", rec.Code)
	}
}
