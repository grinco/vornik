package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type fakeUICredRepo struct{ creds []*persistence.TaskCredential }

func (f *fakeUICredRepo) Upsert(context.Context, *persistence.TaskCredential) error { return nil }
func (f *fakeUICredRepo) ListByTaskLatestExecution(context.Context, string) ([]*persistence.TaskCredential, error) {
	return f.creds, nil
}

// TestTaskDetail_RendersCredentials — a captured tool credential appears in the
// Artifacts area, code-formatted (copyable) with its artifact URL.
func TestTaskDetail_RendersCredentials(t *testing.T) {
	task := &persistence.Task{ID: "task_cred1", ProjectID: "p1", Status: persistence.TaskStatusCompleted}
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
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) { return nil, nil },
	}
	credRepo := &fakeUICredRepo{creds: []*persistence.TaskCredential{
		{TaskID: task.ID, Label: "viewing password", Value: "hunter2-xY9pQ", ArtifactURL: "https://v/p/1"},
	}}

	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithArtifactRepository(artifactRepo),
		WithTaskCredentialRepository(credRepo),
	)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+task.ID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	require.Contains(t, body, "Access credentials")
	require.Contains(t, body, "viewing password")
	require.Contains(t, body, "<code")
	require.Contains(t, body, "hunter2-xY9pQ")
	require.Contains(t, body, "https://v/p/1")
}
