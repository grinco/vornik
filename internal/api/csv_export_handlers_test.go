package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// Mock TaskRepository for CSV export tests
type mockTaskRepository struct {
	listFunc func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error)
}

func (m *mockTaskRepository) Ping(ctx context.Context) error                           { return nil }
func (m *mockTaskRepository) Create(ctx context.Context, task *persistence.Task) error { return nil }
func (m *mockTaskRepository) Get(ctx context.Context, id string) (*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) GetByIdempotencyKey(ctx context.Context, projectID, idempotencyKey string) (*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) Update(ctx context.Context, task *persistence.Task) error { return nil }
func (m *mockTaskRepository) Delete(ctx context.Context, id string) error              { return nil }
func (m *mockTaskRepository) List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, filter)
	}
	return nil, nil
}
func (m *mockTaskRepository) Count(ctx context.Context, filter persistence.TaskFilter) (int64, error) {
	return 0, nil
}
func (m *mockTaskRepository) UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error {
	return nil
}
func (m *mockTaskRepository) TransitionToCancelled(ctx context.Context, id string) (bool, error) {
	return false, nil
}
func (m *mockTaskRepository) RequeueTerminalTask(ctx context.Context, id string, attempt, maxAttempts int) (bool, error) {
	return false, nil
}
func (m *mockTaskRepository) TransitionConditional(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error) {
	return false, nil
}
func (m *mockTaskRepository) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error {
	return nil
}
func (m *mockTaskRepository) ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error {
	return nil
}
func (m *mockTaskRepository) FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error) {
	return nil, nil
}
func (m *mockTaskRepository) CountRecentFailures(ctx context.Context, projectID string, errorClasses []string, since time.Time) (int, error) {
	return 0, nil
}
func (m *mockTaskRepository) GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) CountChildrenForParents(ctx context.Context, parentTaskIDs []string) (map[string]int, error) {
	return nil, nil
}
func (m *mockTaskRepository) GetDependencies(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	return nil, nil
}
func (m *mockTaskRepository) GetDependents(ctx context.Context, taskID string) ([]*persistence.Task, error) {
	return nil, nil
}

// Mock ToolAuditRepository for CSV export tests
type mockToolAuditRepository struct {
	listFunc func(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error)
}

func (m *mockToolAuditRepository) Log(ctx context.Context, entry *persistence.ToolAuditEntry) error {
	return nil
}
func (m *mockToolAuditRepository) List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, filter)
	}
	return nil, nil
}
func (m *mockToolAuditRepository) CountByTool(ctx context.Context, executionID string) (map[string]int64, error) {
	return nil, nil
}

// Mock TaskLLMUsageRepository for CSV export tests
type mockTaskLLMUsageRepository struct {
	listFunc func(ctx context.Context, filter persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error)
}

func (m *mockTaskLLMUsageRepository) Record(ctx context.Context, u *persistence.TaskLLMUsage) error {
	return nil
}
func (m *mockTaskLLMUsageRepository) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	return nil
}
func (m *mockTaskLLMUsageRepository) List(ctx context.Context, filter persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, filter)
	}
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) SumCostByProject(ctx context.Context, projectID string, since, until time.Time) (float64, error) {
	return 0, nil
}
func (m *mockTaskLLMUsageRepository) SumCost(ctx context.Context, since, until time.Time) (float64, error) {
	return 0, nil
}
func (m *mockTaskLLMUsageRepository) AggregateByRoleModel(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) AggregateByProject(ctx context.Context, since, until time.Time, limit int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) AggregateBySource(ctx context.Context, since, until time.Time, projectID string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) TimeSeriesByDay(ctx context.Context, since, until time.Time, projectID string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) TopTasks(ctx context.Context, since, until time.Time, limit int, projectID string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (m *mockTaskLLMUsageRepository) TaskCostBreakdown(ctx context.Context, taskID string) ([]persistence.StepSpend, error) {
	return nil, nil
}

func TestParseCSVTimeRange(t *testing.T) {
	t.Run("returns zero times for empty params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		from, to := parseCSVTimeRange(req)
		assert.True(t, from.IsZero())
		assert.True(t, to.IsZero())
	})

	t.Run("parses RFC3339 from param", func(t *testing.T) {
		fromTime := "2024-01-15T10:00:00Z"
		req := httptest.NewRequest(http.MethodGet, "/?from="+fromTime, nil)
		from, to := parseCSVTimeRange(req)
		assert.False(t, from.IsZero())
		assert.True(t, to.IsZero())
		assert.Equal(t, "2024-01-15 10:00:00 +0000 UTC", from.String())
	})

	t.Run("parses relative duration as from param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?from=-24h", nil)
		from, to := parseCSVTimeRange(req)
		assert.False(t, from.IsZero())
		assert.True(t, to.IsZero())
		expected := time.Now().Add(-24 * time.Hour)
		assert.WithinDuration(t, expected, from, time.Second)
	})

	t.Run("parses RFC3339 to param", func(t *testing.T) {
		toTime := "2024-12-31T23:59:59Z"
		req := httptest.NewRequest(http.MethodGet, "/?to="+toTime, nil)
		from, to := parseCSVTimeRange(req)
		assert.True(t, from.IsZero())
		assert.False(t, to.IsZero())
		assert.Equal(t, "2024-12-31 23:59:59 +0000 UTC", to.String())
	})

	t.Run("ignores invalid from value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?from=invalid", nil)
		from, to := parseCSVTimeRange(req)
		assert.True(t, from.IsZero())
		assert.True(t, to.IsZero())
	})

	t.Run("ignores invalid to value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?to=bad-time", nil)
		from, to := parseCSVTimeRange(req)
		assert.True(t, from.IsZero())
		assert.True(t, to.IsZero())
	})

	t.Run("parses both from and to", func(t *testing.T) {
		fromTime := "2024-01-01T00:00:00Z"
		toTime := "2024-12-31T23:59:59Z"
		req := httptest.NewRequest(http.MethodGet, "/?from="+fromTime+"&to="+toTime, nil)
		from, to := parseCSVTimeRange(req)
		assert.False(t, from.IsZero())
		assert.False(t, to.IsZero())
		assert.Equal(t, "2024-01-01 00:00:00 +0000 UTC", from.String())
		assert.Equal(t, "2024-12-31 23:59:59 +0000 UTC", to.String())
	})
}

func TestParseCSVLimit(t *testing.T) {
	t.Run("returns default when limit is empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})

	t.Run("returns provided valid limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=100", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 100, limit)
	})

	t.Run("returns default for invalid limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=abc", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})

	t.Run("returns default for zero limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})

	t.Run("returns default for negative limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=-5", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})

	t.Run("caps at maxRows when limit exceeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=20000", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})

	t.Run("exactly maxRows returns maxRows", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=10000", nil)
		limit := parseCSVLimit(req)
		assert.Equal(t, 10000, limit)
	})
}

func TestStrPtrOrEmpty(t *testing.T) {
	t.Run("returns empty string for nil pointer", func(t *testing.T) {
		result := strPtrOrEmpty(nil)
		assert.Equal(t, "", result)
	})

	t.Run("returns value for non-nil pointer", func(t *testing.T) {
		s := "test-value"
		result := strPtrOrEmpty(&s)
		assert.Equal(t, "test-value", result)
	})
}

func TestTimePtrOrEmpty(t *testing.T) {
	t.Run("returns empty string for nil pointer", func(t *testing.T) {
		result := timePtrOrEmpty(nil)
		assert.Equal(t, "", result)
	})

	t.Run("returns RFC3339 formatted time for non-nil pointer", func(t *testing.T) {
		now := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		result := timePtrOrEmpty(&now)
		assert.Equal(t, "2024-01-15T10:30:00Z", result)
	})
}

func TestTruncateForCSV(t *testing.T) {
	t.Run("returns short string unchanged", func(t *testing.T) {
		s := "short"
		result := truncateForCSV(s)
		assert.Equal(t, "short", result)
	})

	t.Run("truncates exactly at max length", func(t *testing.T) {
		s := strings.Repeat("x", 4096)
		result := truncateForCSV(s)
		assert.Equal(t, 4096, len(result))
		assert.Equal(t, s, result)
	})

	t.Run("truncates and adds marker for long string", func(t *testing.T) {
		s := strings.Repeat("a", 4100)
		result := truncateForCSV(s)
		assert.Equal(t, 4096+14, len(result)) // 4096 + len("…[truncated]")
		assert.True(t, strings.HasSuffix(result, "…[truncated]"))
	})
}

func TestExportTasksCSV_TaskRepoNotConfigured(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/tasks.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportTasksCSV(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXPORT_DISABLED")
}

func TestExportTasksCSV_ProjectIDRequired(t *testing.T) {
	taskRepo := &mockTaskRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//tasks.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportTasksCSV(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestExportTasksCSV_Success(t *testing.T) {
	now := time.Now()
	taskRepo := &mockTaskRepository{
		listFunc: func(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:             "task-1",
					ProjectID:      "project-1",
					WorkflowID:     strPtr("wf-1"),
					Status:         persistence.TaskStatusCompleted,
					Priority:       50,
					Attempt:        1,
					MaxAttempts:    3,
					MessageCount:   5,
					CurrentPhase:   strPtr("phase-1"),
					CreatedAt:      now,
					UpdatedAt:      now,
					CreationSource: persistence.TaskCreationSourceUser,
				},
			}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/tasks.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportTasksCSV(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/csv; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "tasks-project-1")
	assert.Contains(t, rec.Body.String(), "id,project_id,workflow_id")
	assert.Contains(t, rec.Body.String(), "task-1")
	assert.Contains(t, rec.Body.String(), "project-1")
}

func TestExportAuditCSV_ToolAuditRepoNotConfigured(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/audit.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportAuditCSV(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXPORT_DISABLED")
}

func TestExportAuditCSV_ProjectIDRequired(t *testing.T) {
	toolAuditRepo := &mockToolAuditRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(toolAuditRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//audit.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportAuditCSV(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestExportAuditCSV_Success(t *testing.T) {
	now := time.Now()
	toolAuditRepo := &mockToolAuditRepository{
		listFunc: func(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
			return []*persistence.ToolAuditEntry{
				{
					ID:          "audit-1",
					TaskID:      "task-1",
					ExecutionID: "exec-1",
					StepID:      "step-1",
					ToolName:    "test-tool",
					DurationMs:  1234,
					ToolInput:   "input data",
					ToolOutput:  "output data",
					CreatedAt:   now,
				},
			}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(toolAuditRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/audit.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportAuditCSV(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/csv; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "audit-project-1")
	assert.Contains(t, rec.Body.String(), "id,task_id,execution_id")
	assert.Contains(t, rec.Body.String(), "audit-1")
	assert.Contains(t, rec.Body.String(), "test-tool")
}

func TestExportSpendCSV_LLMUsageRepoNotConfigured(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/spend.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportSpendCSV(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "EXPORT_DISABLED")
}

func TestExportSpendCSV_ProjectIDRequired(t *testing.T) {
	llmUsageRepo := &mockTaskLLMUsageRepository{}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(llmUsageRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects//spend.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportSpendCSV(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VALIDATION_ERROR")
}

func TestExportSpendCSV_Success(t *testing.T) {
	now := time.Now()
	llmUsageRepo := &mockTaskLLMUsageRepository{
		listFunc: func(ctx context.Context, filter persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
			return []*persistence.TaskLLMUsage{
				{
					ID:               "usage-1",
					TaskID:           strPtr("task-1"),
					ExecutionID:      strPtr("exec-1"),
					StepID:           "step-1",
					Role:             "assistant",
					Model:            "claude-3",
					Source:           "workflow_step",
					PromptTokens:     1000,
					CompletionTokens: 500,
					Iterations:       2,
					CostUSD:          0.01,
					RecordedAt:       now,
				},
			}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithLLMUsageRepository(llmUsageRepo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/spend.csv", nil)
	rec := httptest.NewRecorder()

	server.ExportSpendCSV(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/csv; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "spend-project-1")
	assert.Contains(t, rec.Body.String(), "task_id,execution_id,step_id")
	assert.Contains(t, rec.Body.String(), "task-1")
	assert.Contains(t, rec.Body.String(), "assistant")
}

func (m *mockTaskLLMUsageRepository) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (m *mockTaskLLMUsageRepository) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
