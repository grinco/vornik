// Package ui: tests for TaskConversationAction — the multiplexer
// dispatching per-action POST routes (/ui/tasks/{id}/{action}).
// The per-action sub-helpers (uiAnswerCheckpoint / uiAmendBrief /
// uiCloseTask / uiSimpleFlip / uiPostMessage) have their own tests
// in task_conversation_actions_test.go; this file pins the
// multiplexer's branches.
package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func makeTcaServer(taskRepo persistence.TaskRepository) *Server {
	return NewServer(
		WithTaskRepository(taskRepo),
		WithTaskMessageRepository(&uiTcStubMsgRepo{}),
	)
}

func TestTaskConversationAction_MethodNotAllowed(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/t1/pause", nil)
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskConversationAction_LifecycleDisabled(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/pause", nil)
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d", rec.Code)
	}
}

func TestTaskConversationAction_BadPath(t *testing.T) {
	srv := makeTcaServer(&mocks.MockTaskRepository{})
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1", nil)
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestTaskConversationAction_TaskNotFound(t *testing.T) {
	srv := makeTcaServer(&mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return nil, errors.New("not found")
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/pause", nil)
	rec := httptest.NewRecorder()
	srv.TaskConversationAction(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

// taskTransitionPasses returns a mock repo that always succeeds on
// TransitionConditional + returns the task at the given status.
func taskTransitionPasses(t *testing.T, status persistence.TaskStatus) *mocks.MockTaskRepository {
	t.Helper()
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: "t1", ProjectID: "p1", Status: status}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
		RequeueTerminalTaskFunc: func(_ context.Context, _ string, _, _ int) (bool, error) {
			return true, nil
		},
	}
}

// Per-action dispatch fan-out test.
func TestTaskConversationAction_DispatchesActions(t *testing.T) {
	cases := []struct {
		action     string
		status     persistence.TaskStatus
		formExtras url.Values
	}{
		{"message", persistence.TaskStatusAwaitingInput, url.Values{"content": []string{"hi"}}},
		{"directive", persistence.TaskStatusAwaitingInput, url.Values{"content": []string{"do this"}}},
		{"amend", persistence.TaskStatusAwaitingInput, url.Values{"new_brief": []string{"new"}}},
		{"pause", persistence.TaskStatusRunning, url.Values{}},
		{"resume", persistence.TaskStatusPaused, url.Values{}},
		{"close", persistence.TaskStatusCompleted, url.Values{}},
		{"answer", persistence.TaskStatusAwaitingInput, url.Values{
			"checkpoint_id": []string{"cp1"},
			"content":       []string{"yes"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			srv := makeTcaServer(taskTransitionPasses(t, tc.status))
			// For the answer case we also need OpenCheckpointID
			// to satisfy the matcher in uiAnswerCheckpoint.
			if tc.action == "answer" {
				srv = NewServer(
					WithTaskRepository(&mocks.MockTaskRepository{
						GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
							cp := "cp1"
							return &persistence.Task{
								ID:               "t1",
								ProjectID:        "p1",
								Status:           tc.status,
								OpenCheckpointID: &cp,
							}, nil
						},
						TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
							return true, nil
						},
					}),
					WithTaskMessageRepository(&uiTcStubMsgRepo{}),
				)
			}
			req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t1/"+tc.action,
				strings.NewReader(tc.formExtras.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			srv.TaskConversationAction(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Header().Get("Location"), "/ui/tasks/t1") {
				t.Errorf("redirect: got %q", rec.Header().Get("Location"))
			}
		})
	}
}
