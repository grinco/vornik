package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// extendedLLMUsageRepo extends stubLLMUsageRepo with the extra
// aggregation surfaces Spend reads — by-project, by-source, top-tasks,
// time-series.
type extendedLLMUsageRepo struct {
	stubLLMUsageRepo
	bySource           []persistence.SourceSpend
	byProject          []persistence.ProjectSpend
	topTasks           []persistence.TaskSpend
	dailySpend         []persistence.DailySpend
	stepSpend          []persistence.StepSpend
	sourceProjectArgs  []string
	topTaskProjectArgs []string
}

func (s *extendedLLMUsageRepo) AggregateBySource(_ context.Context, _ time.Time, _ time.Time, projectID string) ([]persistence.SourceSpend, error) {
	s.sourceProjectArgs = append(s.sourceProjectArgs, projectID)
	return s.bySource, nil
}
func (s *extendedLLMUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return s.byProject, nil
}
func (s *extendedLLMUsageRepo) TopTasks(_ context.Context, _ time.Time, _ time.Time, _ int, projectID string) ([]persistence.TaskSpend, error) {
	s.topTaskProjectArgs = append(s.topTaskProjectArgs, projectID)
	return s.topTasks, nil
}
func (s *extendedLLMUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return s.dailySpend, nil
}
func (s *extendedLLMUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return s.stepSpend, nil
}

func TestSpend_NoRepoRendersDegraded(t *testing.T) {
	srv := NewServer() // no llmUsageRepo
	req := httptest.NewRequest(http.MethodGet, "/spend", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// Page header still renders.
	assert.Contains(t, rec.Body.String(), "Spend")
}

func TestSpend_HappyPathWith7DayWindow(t *testing.T) {
	repo := &extendedLLMUsageRepo{
		bySource: []persistence.SourceSpend{
			{Source: "agent", CostUSD: 5.0, CallCount: 10, PromptTokens: 1000, CompletionTokens: 200},
			{Source: "dispatcher", CostUSD: 2.0, CallCount: 5, PromptTokens: 500, CompletionTokens: 100},
		},
		byProject: []persistence.ProjectSpend{
			{ProjectID: "p1", CostUSD: 4.0, TaskCount: 3, StepCount: 7, PromptTokens: 1000, CompletionTokens: 200},
		},
		topTasks: []persistence.TaskSpend{
			{TaskID: "task_a", ProjectID: "p1", Status: "COMPLETED", CostUSD: 3.0, StepCount: 2, Iterations: 4,
				FirstStepAt: time.Now().Add(-time.Hour), LastStepAt: time.Now()},
		},
		dailySpend: []persistence.DailySpend{
			{Day: time.Now(), CostUSD: 1.5, CallCount: 3},
		},
	}
	repo.sum = 7.0
	srv := NewServer(WithLLMUsageRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/spend?window=7d", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "p1", "project id should render")
	assert.Contains(t, body, "task_a", "top task id should render")
}

func TestSpend_DefaultsScopedKeyToVisibleProject(t *testing.T) {
	repo := &extendedLLMUsageRepo{
		bySource: []persistence.SourceSpend{{Source: "agent", CostUSD: 1}},
		byProject: []persistence.ProjectSpend{
			{ProjectID: "project-a", CostUSD: 1, TaskCount: 1},
			{ProjectID: "project-b", CostUSD: 99, TaskCount: 1},
		},
		topTasks: []persistence.TaskSpend{{TaskID: "task-a", ProjectID: "project-a", CostUSD: 1}},
	}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"project-a": {ID: "project-a", DisplayName: "A"},
		"project-b": {ID: "project-b", DisplayName: "B"},
	})
	srv := NewServer(WithLLMUsageRepository(repo), WithProjectRegistry(reg))
	req := scopedUIRequest(http.MethodGet, "/spend", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NotEmpty(t, repo.sourceProjectArgs)
	assert.Equal(t, "project-a", repo.sourceProjectArgs[0])
	assert.NotContains(t, rec.Body.String(), "project-b")
}

func TestSpend_RejectsUnauthorizedProjectFilter(t *testing.T) {
	repo := &extendedLLMUsageRepo{}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"project-a": {ID: "project-a", DisplayName: "A"},
		"project-b": {ID: "project-b", DisplayName: "B"},
	})
	srv := NewServer(WithLLMUsageRepository(repo), WithProjectRegistry(reg))
	req := scopedUIRequest(http.MethodGet, "/spend?project=project-b", []string{"project-a"})
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSpend_24hWindowFlipsLabel(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&extendedLLMUsageRepo{}))
	req := httptest.NewRequest(http.MethodGet, "/spend?window=24h", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "24 hours")
}

func TestSpend_30dWindowFlipsLabel(t *testing.T) {
	srv := NewServer(WithLLMUsageRepository(&extendedLLMUsageRepo{}))
	req := httptest.NewRequest(http.MethodGet, "/spend?window=30d", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "30 days")
}

func TestSpend_FiltersByProject(t *testing.T) {
	repo := &extendedLLMUsageRepo{
		byProject: []persistence.ProjectSpend{
			{ProjectID: "p1", CostUSD: 5.0, TaskCount: 2, PromptTokens: 100, CompletionTokens: 50},
			{ProjectID: "p2", CostUSD: 3.0, TaskCount: 1, PromptTokens: 100, CompletionTokens: 50},
		},
	}
	srv := NewServer(WithLLMUsageRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/spend?project=p1", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	// p1 row renders.
	assert.Contains(t, body, "p1")
}

func TestSpend_ProjectsListPopulatesFromRegistry(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	srv.llmUsageRepo = &extendedLLMUsageRepo{}
	req := httptest.NewRequest(http.MethodGet, "/spend", nil)
	rec := httptest.NewRecorder()
	srv.Spend(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "p1")
}
