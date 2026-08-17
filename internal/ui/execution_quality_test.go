package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type fakeUIExecutionQualityRepo struct {
	score         *persistence.ExecutionQualityScore
	err           error
	rows          []*persistence.ExecutionQualityScore
	filter        persistence.ExecutionQualityScoreFilter
	pending       persistence.ExecutionQualityPendingStats
	statsProjects []string
}

func (f *fakeUIExecutionQualityRepo) Upsert(context.Context, *persistence.ExecutionQualityScore) error {
	return nil
}
func (f *fakeUIExecutionQualityRepo) GetByExecution(context.Context, string) (*persistence.ExecutionQualityScore, error) {
	return f.score, f.err
}
func (f *fakeUIExecutionQualityRepo) List(_ context.Context, filter persistence.ExecutionQualityScoreFilter) ([]*persistence.ExecutionQualityScore, error) {
	f.filter = filter
	return f.rows, nil
}
func (f *fakeUIExecutionQualityRepo) ListPendingTerminal(context.Context, int) ([]*persistence.Execution, error) {
	return nil, nil
}
func (f *fakeUIExecutionQualityRepo) PendingTerminalStats(_ context.Context, projects []string) (persistence.ExecutionQualityPendingStats, error) {
	f.statsProjects = append([]string(nil), projects...)
	return f.pending, nil
}

func renderExecutionQuality(t *testing.T, repo persistence.ExecutionQualityScoreRepository) string {
	t.Helper()
	exec := &persistence.Execution{
		ID: "exec-quality", TaskID: "task-quality", ProjectID: "p1", WorkflowID: "dev-pipeline",
		Status: persistence.ExecutionStatusFailed, CompletedSteps: []string{"analyze", "test"},
	}
	srv := NewServer(
		WithExecutionRepository(&mocks.MockExecutionRepository{GetFunc: func(context.Context, string) (*persistence.Execution, error) { return exec, nil }}),
		WithTaskRepository(&mocks.MockTaskRepository{GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return &persistence.Task{ID: exec.TaskID, ProjectID: exec.ProjectID}, nil
		}}),
		WithExecutionQualityScoreRepository(repo),
	)
	rec := httptest.NewRecorder()
	srv.ExecutionDetail(rec, httptest.NewRequest(http.MethodGet, "/executions/"+exec.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestExecutionDetail_RendersGradedQualityAndActionLinks(t *testing.T) {
	score := 0.5
	body := renderExecutionQuality(t, &fakeUIExecutionQualityRepo{score: &persistence.ExecutionQualityScore{
		ExecutionID: "exec-quality", Status: "scored", Score: &score,
		PassedCaseCount: 1, PinnedCaseCount: 2,
	}})
	for _, want := range []string{"Execution quality", "50%", "1 of 2 pinned cases", "Replay", "Rerun"} {
		if !strings.Contains(body, want) {
			t.Errorf("quality detail missing %q", want)
		}
	}
}

func TestExecutionDetail_ShowsPublicationPendingInsteadOfSilentHole(t *testing.T) {
	body := renderExecutionQuality(t, &fakeUIExecutionQualityRepo{err: persistence.ErrNotFound})
	if !strings.Contains(body, "Publication pending") || !strings.Contains(body, "terminal execution has no score row yet") {
		t.Fatalf("pending publication not visible in body")
	}
}

func TestExecutionDetail_ShowsInvalidEvidenceDiagnostic(t *testing.T) {
	zero := 0.0
	body := renderExecutionQuality(t, &fakeUIExecutionQualityRepo{score: &persistence.ExecutionQualityScore{
		ExecutionID: "exec-quality", Status: "invalid_evidence", Score: &zero,
		Diagnostic: "unknown_case_id",
	}})
	if !strings.Contains(body, "Invalid evidence") || !strings.Contains(body, "unknown_case_id") {
		t.Fatalf("invalid evidence reason not actionable")
	}
}

func TestInsightsQuality_ScopesAggregatesAndLinksLowScoresToActions(t *testing.T) {
	one, half, zero := 1.0, 0.5, 0.0
	repo := &fakeUIExecutionQualityRepo{
		rows: []*persistence.ExecutionQualityScore{
			{ProjectID: "p1", ExecutionID: "e-high", WorkflowID: "dev-pipeline", Status: "scored", Score: &one},
			{ProjectID: "p1", ExecutionID: "e-low", WorkflowID: "dev-pipeline", Status: "scored", Score: &half},
			{ProjectID: "p1", ExecutionID: "e-invalid", WorkflowID: "dev-pipeline", Status: "invalid_evidence", Score: &zero, Diagnostic: "unknown_case_id"},
		},
		pending: persistence.ExecutionQualityPendingStats{Count: 2},
	}
	srv := NewServer(WithExecutionQualityScoreRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/insights/quality?projectId=p1", nil)
	req = req.WithContext(api.ContextWithProjectScope(req.Context(), "p1"))
	rec := httptest.NewRecorder()
	srv.InsightsQuality(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.filter.ProjectIDs) != 1 || repo.filter.ProjectIDs[0] != "p1" || len(repo.statsProjects) != 1 || repo.statsProjects[0] != "p1" {
		t.Fatalf("quality insights escaped project scope: filter=%+v stats=%v", repo.filter, repo.statsProjects)
	}
	body := rec.Body.String()
	for _, want := range []string{"Execution quality", "75%", "Publication pending", "2", "/ui/executions/e-low", "unknown_case_id"} {
		if !strings.Contains(body, want) {
			t.Errorf("quality insights missing %q", want)
		}
	}
}
