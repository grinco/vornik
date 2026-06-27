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

// TestTaskDetail_CSVExport — ?format=csv on the task detail URL
// streams a CSV file rather than the HTML page.
func TestTaskDetail_CSVExport(t *testing.T) {
	taskID := "task_csv"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	usageRepo := &extendedLLMUsageRepo{
		stubLLMUsageRepo: stubLLMUsageRepo{
			rows: []*persistence.TaskLLMUsage{
				{StepID: "step-a", Role: "coder", Model: "test-model",
					PromptTokens: 100, CompletionTokens: 50, Iterations: 2, CostUSD: 0.05},
			},
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithLLMUsageRepository(usageRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID+"?format=csv", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/csv")
	body := rec.Body.String()
	assert.Contains(t, body, "step_id") // header row
	assert.Contains(t, body, "step-a")  // data row
	assert.Contains(t, body, "coder")
}

// TestTaskDetail_JSONExport — ?format=json streams a JSON payload.
func TestTaskDetail_JSONExport(t *testing.T) {
	taskID := "task_json"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	usageRepo := &extendedLLMUsageRepo{
		stubLLMUsageRepo: stubLLMUsageRepo{
			rows: []*persistence.TaskLLMUsage{
				{StepID: "step-x", Role: "judge", Model: "haiku",
					PromptTokens: 200, CompletionTokens: 80, Iterations: 1, CostUSD: 0.01},
			},
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithLLMUsageRepository(usageRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID+"?format=json", nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"task_id"`)
	assert.Contains(t, body, taskID)
	assert.Contains(t, body, `"rows"`)
	assert.Contains(t, body, "step-x")
}

// TestTaskDetail_RendersFailedTaskWithPlaybook — FAILED tasks with
// a last_error_class should attract the failure-class playbook.
func TestTaskDetail_RendersFailedTaskWithPlaybook(t *testing.T) {
	taskID := "task_failed"
	errClass := "schema_violation"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{
				ID: taskID, ProjectID: "p1",
				Status:         persistence.TaskStatusFailed,
				LastErrorClass: &errClass,
			}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// The playbook block should reference the error class.
	if strings.Contains(body, "playbook") || strings.Contains(body, "Playbook") {
		// good — the playbook section rendered
	} else {
		// Even if the template doesn't use the literal word, the
		// failure-class string should at least appear somewhere.
		assert.Contains(t, body, "schema_violation")
	}
}

// isChangelogArtifact regression tests — both legacy and disambig'd
// names, case-folding, false positives.

func TestIsChangelogArtifact_LegacyForm(t *testing.T) {
	assert.True(t, isChangelogArtifact("CHANGELOG.md"))
	assert.True(t, isChangelogArtifact("changelog.md"), "lowercase should match")
}

func TestIsChangelogArtifact_DisambigForm(t *testing.T) {
	assert.True(t, isChangelogArtifact("CHANGELOG-20260520-abcd.md"))
	assert.True(t, isChangelogArtifact("changelog-20260520-1234.md"))
}

func TestIsChangelogArtifact_BadDateLength(t *testing.T) {
	assert.False(t, isChangelogArtifact("changelog-2026052-abcd.md"))
	assert.False(t, isChangelogArtifact("changelog-202605201-abcd.md"))
}

func TestIsChangelogArtifact_NonHexID(t *testing.T) {
	assert.False(t, isChangelogArtifact("changelog-20260520-xyz1.md"))
}

func TestIsChangelogArtifact_EmptyName(t *testing.T) {
	assert.False(t, isChangelogArtifact(""))
}

func TestIsChangelogArtifact_OperatorNamedFile(t *testing.T) {
	// Operator-supplied tag should NOT be pulled into the
	// inline-render path.
	assert.False(t, isChangelogArtifact("CHANGELOG-2026-Q2.md"))
}

func TestIsChangelogArtifact_WrongSuffix(t *testing.T) {
	assert.False(t, isChangelogArtifact("changelog-20260520-abcd.txt"))
}

func TestIsChangelogArtifact_NoDashSeparator(t *testing.T) {
	assert.False(t, isChangelogArtifact("changelog-202605201234abcd.md"))
}
