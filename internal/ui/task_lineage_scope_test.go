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

// A cross-project call (executor/call_project.go) creates the callee task with
// ProjectID = the TARGET project but ParentTaskID = the CALLER's task. So
// tasks.parent_task_id legitimately spans projects, and TaskRepository.
// GetChildren carries no project predicate — it returns those children too.
//
// That makes project identity a REQUIRED part of the descendant walk. Rolling
// artifacts up the lineage without it would surface another project's artifact
// names on this page, and — worse — the relaxed send guard would let a caller
// authorized for only this project deliver another project's artifact. The
// caller's project scope is checked against the PARENT task
// (uiRequireProjectScope), so nothing downstream re-checks the child.
func crossProjectLineage() (parent, sameProjChild, otherProjChild *persistence.Task) {
	parent = &persistence.Task{
		ID: "task_caller", ProjectID: "proj-a",
		ChatTurnID: strpDS("chat_1"), Status: persistence.TaskStatusCompleted,
	}
	sameProjChild = &persistence.Task{
		ID: "task_child_same", ProjectID: "proj-a",
		ParentTaskID: &parent.ID, Status: persistence.TaskStatusCompleted,
	}
	otherProjChild = &persistence.Task{
		ID: "task_child_other", ProjectID: "proj-b", // cross-project callee
		ParentTaskID: &parent.ID, Status: persistence.TaskStatusCompleted,
	}
	return parent, sameProjChild, otherProjChild
}

func crossProjectRepos(parent, same, other *persistence.Task) (*mocks.MockTaskRepository, *persistence.Artifact, *persistence.Artifact) {
	sameArt := &persistence.Artifact{
		ID: "art_same", Name: "ours-report.html", TaskID: &same.ID,
		ProjectID: "proj-a", ArtifactClass: persistence.ArtifactClassOutput,
	}
	otherArt := &persistence.Artifact{
		ID: "art_other", Name: "other-tenant-secret.html", TaskID: &other.ID,
		ProjectID: "proj-b", ArtifactClass: persistence.ArtifactClassOutput,
	}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			for _, t := range []*persistence.Task{parent, same, other} {
				if t.ID == id {
					return t, nil
				}
			}
			return nil, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == parent.ID {
				// GetChildren has no project predicate — both come back.
				return []*persistence.Task{same, other}, nil
			}
			return nil, nil
		},
	}
	return taskRepo, sameArt, otherArt
}

// TestTaskDetail_DoesNotRollUpCrossProjectChildArtifacts — the roll-up must stop
// at a project boundary, or a cross-project callee's artifact names leak onto
// the caller's page.
func TestTaskDetail_DoesNotRollUpCrossProjectChildArtifacts(t *testing.T) {
	parent, same, other := crossProjectLineage()
	taskRepo, sameArt, otherArt := crossProjectRepos(parent, same, other)

	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if filter.TaskID == nil {
				return nil, nil
			}
			switch *filter.TaskID {
			case same.ID:
				return []*persistence.Artifact{sameArt}, nil
			case other.ID:
				return []*persistence.Artifact{otherArt}, nil
			}
			return nil, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: "exec1", TaskID: parent.ID, Status: persistence.ExecutionStatusCompleted}}, nil
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

	assert.Contains(t, body, "ours-report.html",
		"a same-project child's deliverable must still roll up")
	assert.NotContains(t, body, "other-tenant-secret.html",
		"a cross-project callee's artifact must NOT appear on the caller's task page")
}

// TestDeliverableSend_RejectsCrossProjectDescendantArtifact — the send guard
// must not treat a cross-project descendant as in scope. The caller was
// authorized against the PARENT's project only.
func TestDeliverableSend_RejectsCrossProjectDescendantArtifact(t *testing.T) {
	parent, same, other := crossProjectLineage()
	taskRepo, _, otherArt := crossProjectRepos(parent, same, other)

	artifactRepo := &mocks.MockArtifactRepository{
		GetFunc: func(context.Context, string) (*persistence.Artifact, error) { return otherArt, nil },
	}
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "slack:T03/D0B#main", ProjectID: "proj-a"}
	ch := &fakeChannelDS{name: "slack"}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithArtifactRepository(artifactRepo),
		WithChatAuditRepository(fakeChatAuditDS{row: row}),
		WithChannelResolver(fakeResolverDS{byName: map[string]conversation.Channel{"slack": ch}}),
	)

	req := httptest.NewRequest(http.MethodPost,
		"/tasks/"+parent.ID+"/artifacts/"+otherArt.ID+"/send", nil)
	rec := httptest.NewRecorder()
	srv.DeliverableSend(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a cross-project descendant's artifact must not be sendable from the caller's task page")
	assert.Empty(t, ch.sent, "nothing may be delivered for a cross-project artifact")
}
