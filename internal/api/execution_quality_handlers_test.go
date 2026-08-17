package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type fakeAPIExecutionQualityRepo struct {
	filter       persistence.ExecutionQualityScoreFilter
	statsProject []string
	rows         []*persistence.ExecutionQualityScore
	stats        persistence.ExecutionQualityPendingStats
	listCalls    int
}

func (f *fakeAPIExecutionQualityRepo) Upsert(context.Context, *persistence.ExecutionQualityScore) error {
	return nil
}
func (f *fakeAPIExecutionQualityRepo) GetByExecution(context.Context, string) (*persistence.ExecutionQualityScore, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakeAPIExecutionQualityRepo) List(_ context.Context, filter persistence.ExecutionQualityScoreFilter) ([]*persistence.ExecutionQualityScore, error) {
	f.listCalls++
	f.filter = filter
	return f.rows, nil
}
func (f *fakeAPIExecutionQualityRepo) ListPendingTerminal(context.Context, int) ([]*persistence.Execution, error) {
	return nil, nil
}
func (f *fakeAPIExecutionQualityRepo) PendingTerminalStats(_ context.Context, projects []string) (persistence.ExecutionQualityPendingStats, error) {
	f.statsProject = append([]string(nil), projects...)
	return f.stats, nil
}

func TestExecutionQualityList_ProjectScopedAndShowsPublicationBacklog(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	repo := &fakeAPIExecutionQualityRepo{
		rows: []*persistence.ExecutionQualityScore{{
			ProjectID: "p1", ExecutionID: "e1", TaskID: "t1", WorkflowID: "dev-pipeline",
			Status: "scored", Score: apiFloatPtr(0.5), RecordedAt: now,
		}},
		stats: persistence.ExecutionQualityPendingStats{Count: 2, OldestAt: &now},
	}
	srv := NewServer(WithExecutionQualityScoreRepository(repo))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/quality/executions?project_id=p1&status=scored&max_score=0.7&limit=25", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "p1"))
	rec := httptest.NewRecorder()
	srv.ExecutionQualityList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if repo.listCalls != 1 || len(repo.filter.ProjectIDs) != 1 || repo.filter.ProjectIDs[0] != "p1" ||
		len(repo.filter.Statuses) != 1 || repo.filter.Statuses[0] != "scored" || repo.filter.MaxScore == nil ||
		*repo.filter.MaxScore != 0.7 || repo.filter.PageSize != 25 {
		t.Fatalf("unsafe/wrong repository filter: %+v", repo.filter)
	}
	if len(repo.statsProject) != 1 || repo.statsProject[0] != "p1" {
		t.Fatalf("pending stats were not project scoped: %v", repo.statsProject)
	}
	var body struct {
		Count       int                                      `json:"count"`
		Publication persistence.ExecutionQualityPendingStats `json:"publication"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || body.Publication.Count != 2 || body.Publication.OldestAt == nil {
		t.Fatalf("response = %+v", body)
	}
}

func TestExecutionQualityList_RejectsCrossProjectBeforeQuery(t *testing.T) {
	repo := &fakeAPIExecutionQualityRepo{}
	srv := NewServer(WithExecutionQualityScoreRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/quality/executions?project_id=p2", nil)
	req = req.WithContext(ContextWithProjectScope(req.Context(), "p1"))
	rec := httptest.NewRecorder()
	srv.ExecutionQualityList(rec, req)
	if rec.Code != http.StatusForbidden || repo.listCalls != 0 {
		t.Fatalf("cross-project request status=%d listCalls=%d body=%s", rec.Code, repo.listCalls, rec.Body.String())
	}
}

func TestExecutionQualityList_RequiresProject(t *testing.T) {
	repo := &fakeAPIExecutionQualityRepo{}
	srv := NewServer(WithExecutionQualityScoreRepository(repo))
	rec := httptest.NewRecorder()
	srv.ExecutionQualityList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/quality/executions", nil))
	if rec.Code != http.StatusBadRequest || repo.listCalls != 0 {
		t.Fatalf("missing-project status=%d listCalls=%d", rec.Code, repo.listCalls)
	}
}

func apiFloatPtr(v float64) *float64 { return &v }
