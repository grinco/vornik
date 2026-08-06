package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// The 2026-08-05 janka-companion shape, reused by both tests below.
//
// A Slack request became a routing task whose only artifacts were two ~300-byte
// router responses. The router delegated the real work to a child task, which
// produced the actual 31 KB deliverable. The parent's artifact list is filtered
// on task_id alone, so the operator saw only the two router scraps and
// concluded the report had never been produced.
const (
	jankaParentTask = "task_20260805170431_955e423704153971"
	jankaChildTask  = "task_20260805170522_ab6d9a44d42779b5"
	jankaReportArt  = "artifact_20260805211355_e53565f9fca24670"
)

func jankaLineage() (parent *persistence.Task, child *persistence.Task) {
	turn := "chat_20260805170412_9dc37555d22e8c76"
	parent = &persistence.Task{
		ID: jankaParentTask, ProjectID: "companion-janka",
		ChatTurnID: &turn, Status: persistence.TaskStatusCompleted,
	}
	child = &persistence.Task{
		ID: jankaChildTask, ProjectID: "companion-janka",
		ParentTaskID: &parent.ID, Status: persistence.TaskStatusCompleted,
	}
	return parent, child
}

// routerArtifacts are what the PARENT task owns: the two tiny router responses.
func routerArtifacts() []*persistence.Artifact {
	pid := jankaParentTask
	return []*persistence.Artifact{
		{ID: "art_route", Name: "route-response-20260805-94d2.md", TaskID: &pid,
			ProjectID: "companion-janka", ArtifactClass: persistence.ArtifactClassOutput},
	}
}

// reportArtifact is what the CHILD task owns: the deliverable that matters.
func reportArtifact() *persistence.Artifact {
	cid := jankaChildTask
	return &persistence.Artifact{
		ID: jankaReportArt, Name: "report-20260805-9bac.html", TaskID: &cid,
		ProjectID: "companion-janka", ArtifactClass: persistence.ArtifactClassOutput,
	}
}

// TestTaskDetail_ListsDescendantArtifacts pins the fix for the 2026-08-05
// lost-deliverable incident: a parent task's Artifacts panel must include the
// artifacts its delegated children produced, or the page claims the work
// produced nothing but two router scraps.
func TestTaskDetail_ListsDescendantArtifacts(t *testing.T) {
	parent, child := jankaLineage()
	report := reportArtifact()

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			switch id {
			case parent.ID:
				return parent, nil
			case child.ID:
				return child, nil
			}
			return nil, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == parent.ID {
				return []*persistence.Task{child}, nil
			}
			return nil, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: "exec_94d2", TaskID: parent.ID, Status: persistence.ExecutionStatusCompleted}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if filter.TaskID == nil {
				return nil, nil
			}
			switch *filter.TaskID {
			case parent.ID:
				return routerArtifacts(), nil
			case child.ID:
				return []*persistence.Artifact{report}, nil
			}
			return nil, nil
		},
	}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithArtifactRepository(artifactRepo),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"slack": nil}}),
	)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+parent.ID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "route-response-20260805-94d2.md", "the parent's own artifacts must still be listed")
	assert.Contains(t, body, "report-20260805-9bac.html",
		"the child's deliverable must be listed on the parent page — otherwise the real output is invisible")
	assert.Contains(t, body, "/ui/tasks/"+parent.ID+"/artifacts/"+jankaReportArt+"/send",
		"the rolled-up child artifact must be sendable from the parent page")
}

// TestDeliverableSend_AcceptsDescendantArtifact — the Send guard rejected any
// artifact whose task_id differed from the page's task, so even once the child
// artifact is listed on the parent page, sending it 404'd.
func TestDeliverableSend_AcceptsDescendantArtifact(t *testing.T) {
	parent, child := jankaLineage()
	report := reportArtifact()

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			switch id {
			case parent.ID:
				return parent, nil
			case child.ID:
				return child, nil
			}
			return nil, nil
		},
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == parent.ID {
				return []*persistence.Task{child}, nil
			}
			return nil, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Artifact, error) {
			if id == report.ID {
				return report, nil
			}
			return nil, nil
		},
	}
	row := &persistence.ChatAuditEntry{
		ID: "chat_20260805170412_9dc37555d22e8c76", ChatID: "slack:T03/D0B#main", ProjectID: "companion-janka",
	}
	ch := &fakeChannelDS{name: "slack"}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: row}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"slack": ch}}),
	)

	req := httptest.NewRequest(http.MethodPost,
		"/tasks/"+parent.ID+"/artifacts/"+report.ID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"a descendant's OUTPUT artifact must be sendable from the ancestor task page")
	require.Len(t, ch.sent, 1, "the child's deliverable should have been sent to the parent's chat")
}

// TestDeliverableSend_UnrelatedTaskArtifact_StillNotFound guards the security
// property the old strict equality provided: relaxing the guard to descendants
// must NOT let a caller send an artifact from an unrelated task by guessing its
// id.
func TestDeliverableSend_UnrelatedTaskArtifact_StillNotFound(t *testing.T) {
	parent, _ := jankaLineage()
	strangerID := "task_stranger"
	stranger := &persistence.Artifact{
		ID: "art_stranger", Name: "secret.md", TaskID: &strangerID,
		ProjectID: "companion-janka", ArtifactClass: persistence.ArtifactClassOutput,
	}

	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			if id == parent.ID {
				return parent, nil
			}
			return nil, nil
		},
		GetChildrenFunc: func(context.Context, string) ([]*persistence.Task, error) {
			return nil, nil // parent has no children
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return stranger, nil },
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
	)

	req := httptest.NewRequest(http.MethodPost,
		"/tasks/"+parent.ID+"/artifacts/"+stranger.ID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"an artifact from an unrelated task must remain unsendable")
}
