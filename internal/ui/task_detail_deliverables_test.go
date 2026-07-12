package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// taskDetailBody renders the task-detail page for a task whose artifacts the
// mock returns, with an optional chat origin (ChatTurnID) and channel resolver.
func taskDetailBody(t *testing.T, task *persistence.Task, arts []*persistence.Artifact, withResolver bool) string {
	t.Helper()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc:  func(context.Context, string) (*persistence.Task, error) { return task, nil },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: "exec1", TaskID: task.ID, Status: persistence.ExecutionStatusCompleted}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			// The merged Artifacts panel reads the task-level list (TaskID
			// filter, ExecutionID nil).
			if filter.ExecutionID == nil {
				return arts, nil
			}
			return arts, nil
		},
	}
	opts := []ServerOption{
		WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo),
	}
	if withResolver {
		opts = append(opts, WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"telegram": nil}}))
	}
	srv := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+task.ID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func chatTask(id, turn string) *persistence.Task {
	t := &persistence.Task{ID: id, ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	if turn != "" {
		t.ChatTurnID = &turn
	}
	return t
}

// TestTaskDetail_MergedArtifacts_SinglePanel — the deliverable + "All
// Artifacts" panels are gone; there is one Artifacts panel and OUTPUT
// artifacts still carry the send action (for a chat-originated task).
func TestTaskDetail_MergedArtifacts_SinglePanel(t *testing.T) {
	arts := []*persistence.Artifact{
		{ID: "out1", Name: "final-report.md", ArtifactClass: persistence.ArtifactClassOutput},
	}
	body := taskDetailBody(t, chatTask("task_m1", "turn-1"), arts, true)

	require.Contains(t, body, "final-report.md")
	// The separate Deliverable panel + legacy "All Artifacts" disclosure are gone.
	require.NotContains(t, body, "deliverables-panel")
	require.NotContains(t, body, "All Artifacts (")
	// OUTPUT artifact carries the send-to-chat action in the Artifacts panel.
	require.Contains(t, body, "/ui/tasks/task_m1/artifacts/out1/send")
}

// TestTaskDetail_NoChatOrigin_NoSendOffer — a webhook/automation task (no
// ChatTurnID) shows the OUTPUT artifact but no send offer.
func TestTaskDetail_NoChatOrigin_NoSendOffer(t *testing.T) {
	arts := []*persistence.Artifact{
		{ID: "out1", Name: "final-report.md", ArtifactClass: persistence.ArtifactClassOutput},
	}
	body := taskDetailBody(t, chatTask("task_wh", ""), arts, true) // resolver wired, but no chat origin
	require.Contains(t, body, "final-report.md")
	require.NotContains(t, body, "/artifacts/out1/send", "no chat origin → no send offer")
}

// TestTaskDetail_NoResolver_NoSendOffer — without a channel resolver wired
// the offer is hidden even for a chat-originated task.
func TestTaskDetail_NoResolver_NoSendOffer(t *testing.T) {
	arts := []*persistence.Artifact{
		{ID: "out1", Name: "final-report.md", ArtifactClass: persistence.ArtifactClassOutput},
	}
	body := taskDetailBody(t, chatTask("task_nr", "turn-1"), arts, false)
	require.NotContains(t, body, "/artifacts/out1/send", "no resolver → no send offer")
}

// TestTaskDetail_SendOnlyOnOutput — INPUT/INTERMEDIATE artifacts never get a
// send action, even for a chat-originated task with a resolver.
func TestTaskDetail_SendOnlyOnOutput(t *testing.T) {
	arts := []*persistence.Artifact{
		{ID: "in1", Name: "input.json", ArtifactClass: persistence.ArtifactClassInput},
		{ID: "mid1", Name: "scratch.txt", ArtifactClass: persistence.ArtifactClassIntermediate},
		{ID: "out1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
	}
	body := taskDetailBody(t, chatTask("task_o1", "turn-1"), arts, true)
	require.Contains(t, body, "/ui/tasks/task_o1/artifacts/out1/send", "OUTPUT gets the send action")
	require.NotContains(t, body, "/artifacts/in1/send", "INPUT must not be sendable")
	require.NotContains(t, body, "/artifacts/mid1/send", "INTERMEDIATE must not be sendable")
}
