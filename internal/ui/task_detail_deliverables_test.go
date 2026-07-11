package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestTaskDetail_DeliverableCards_RenderFirst pins §5.8: a COMPLETED
// task's OUTPUT-class artifacts render as deliverable cards, and they
// appear before the technical "Show technical details" section.
func TestTaskDetail_DeliverableCards_RenderFirst(t *testing.T) {
	taskID := "task_deliv1"
	execID := "exec_deliv1"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return task, nil },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: execID, TaskID: taskID, Status: persistence.ExecutionStatusCompleted}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if filter.ExecutionID != nil && *filter.ExecutionID == execID {
				return []*persistence.Artifact{
					{ID: "out1", Name: "final-report.md", ArtifactClass: persistence.ArtifactClassOutput},
				}, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	require.Contains(t, body, "final-report.md")
	deliverIdx := strings.Index(body, "final-report.md")
	techIdx := strings.Index(body, "Show technical details")
	require.GreaterOrEqual(t, deliverIdx, 0)
	require.GreaterOrEqual(t, techIdx, 0)
	assert.Less(t, deliverIdx, techIdx, "deliverable card must render before the technical details disclosure")
	assert.Contains(t, body, "/ui/tasks/"+taskID+"/artifacts/out1/send", "card should carry the send-to-chat action")
}

// TestTaskDetail_NoOutputArtifacts_FallsBackToCompletionText pins the
// §5.8 fallback: no OUTPUT artifacts → the last story line stands in as
// the deliverable.
func TestTaskDetail_NoOutputArtifacts_FallsBackToCompletionText(t *testing.T) {
	taskID := "task_deliv2"
	execID := "exec_deliv2"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc:  func(context.Context, string) (*persistence.Task, error) { return task, nil },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: execID, TaskID: taskID, Status: persistence.ExecutionStatusCompleted}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return nil, nil // no artifacts at all
		},
	}
	narrationRepo := &fakeNarrationRepoDeliv{
		lines: []*persistence.ExecutionNarration{
			{ExecutionID: execID, Seq: 1, Text: "Started the analysis."},
			{ExecutionID: execID, Seq: 2, Text: "All done — produced the summary."},
		},
	}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithArtifactRepository(artifactRepo),
		WithExecutionNarrationRepository(narrationRepo),
	)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "All done — produced the summary.", "no-OUTPUT-artifact completion should fall back to the last story line")
	assert.NotContains(t, body, "No deliverable recorded for this task.", "fallback text path should take precedence over the generic empty state")
}

// TestTaskDetail_DeliverableCards_ExcludeInputAndIntermediate pins that
// INPUT/INTERMEDIATE artifacts never appear as deliverable cards.
func TestTaskDetail_DeliverableCards_ExcludeInputAndIntermediate(t *testing.T) {
	taskID := "task_deliv3"
	execID := "exec_deliv3"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc:  func(context.Context, string) (*persistence.Task, error) { return task, nil },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{ID: execID, TaskID: taskID, Status: persistence.ExecutionStatusCompleted}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, filter persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			if filter.ExecutionID != nil && *filter.ExecutionID == execID {
				return []*persistence.Artifact{
					{ID: "in1", Name: "input-brief.txt", ArtifactClass: persistence.ArtifactClassInput},
					{ID: "mid1", Name: "scratch-notes.tmp", ArtifactClass: persistence.ArtifactClassIntermediate},
					{ID: "out1", Name: "final.md", ArtifactClass: persistence.ArtifactClassOutput},
				}, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// The deliverables panel spans from "Deliverable</h2>" (well, the
	// sectionHeader) to the next major panel boundary; simplest robust
	// check: the deliverable card send-action only exists for the OUTPUT
	// artifact's ID, never for the INPUT/INTERMEDIATE ones.
	assert.Contains(t, body, "/ui/tasks/"+taskID+"/artifacts/out1/send")
	assert.NotContains(t, body, "/ui/tasks/"+taskID+"/artifacts/in1/send")
	assert.NotContains(t, body, "/ui/tasks/"+taskID+"/artifacts/mid1/send")
}

// TestTaskDetail_NonCompletedTask_NoDeliverablesPanel pins that the
// deliverable-cards panel only renders for COMPLETED tasks.
func TestTaskDetail_NonCompletedTask_NoDeliverablesPanel(t *testing.T) {
	taskID := "task_deliv4"
	task := &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusRunning}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc:  func(context.Context, string) (*persistence.Task, error) { return task, nil },
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `id="deliverables-panel"`)
}

// fakeNarrationRepoDeliv is a minimal ExecutionNarrationRepository stub
// returning fixed lines for the story-panel/fallback test above.
type fakeNarrationRepoDeliv struct {
	lines []*persistence.ExecutionNarration
}

func (f *fakeNarrationRepoDeliv) Insert(context.Context, *persistence.ExecutionNarration) (int64, error) {
	return 0, nil
}

func (f *fakeNarrationRepoDeliv) ListByExecution(_ context.Context, executionID string) ([]*persistence.ExecutionNarration, error) {
	var out []*persistence.ExecutionNarration
	for _, l := range f.lines {
		if l.ExecutionID == executionID {
			out = append(out, l)
		}
	}
	return out, nil
}
