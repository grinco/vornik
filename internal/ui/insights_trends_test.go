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
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestSummarizeTaskTrends_Buckets(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	tasks := []*persistence.Task{
		{CreatedAt: now, Status: persistence.TaskStatusCompleted},
		{CreatedAt: now, Status: persistence.TaskStatusFailed},
		{CreatedAt: now.AddDate(0, 0, -1), Status: persistence.TaskStatusCompleted},
		{CreatedAt: now.AddDate(0, 0, -20), Status: persistence.TaskStatusCompleted}, // outside 14d window
		{CreatedAt: now, Status: persistence.TaskStatusRunning},                      // throughput only
	}
	s := summarizeTaskTrends(tasks, now, 14)

	require.Len(t, s.Buckets, 14)
	today := s.Buckets[len(s.Buckets)-1]
	assert.Equal(t, "2026-06-17", today.Date)
	assert.Equal(t, 3, today.Created, "2 terminal + 1 running created today")
	assert.Equal(t, 1, today.Completed)
	assert.Equal(t, 1, today.Failed)
	assert.Equal(t, 50, today.SuccessPct, "1/(1+1)")

	// Totals exclude the 20-days-ago task.
	assert.Equal(t, 4, s.TotCreated)
	assert.Equal(t, 2, s.TotCompleted)
	assert.Equal(t, 1, s.TotFailed)
	assert.Equal(t, 66, s.OverallSuccessPct, "2/(2+1) → 66")
}

func TestSummarizeTaskTrends_Empty(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	s := summarizeTaskTrends(nil, now, 14)
	require.Len(t, s.Buckets, 14)
	assert.Equal(t, 0, s.TotCreated)
	assert.Equal(t, 0, s.OverallSuccessPct)
}

func TestInsightsTrends_RendersChart(t *testing.T) {
	now := time.Now().UTC()
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{CreatedAt: now, Status: persistence.TaskStatusCompleted},
				{CreatedAt: now, Status: persistence.TaskStatusFailed},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/trends", nil)
	rec := httptest.NewRecorder()
	srv.InsightsTrends(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "<svg")
	assert.Contains(t, body, "Success rate")
}

func TestInsightsTrends_NilRepoShowsNotice(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/ui/insights/trends", nil)
	rec := httptest.NewRecorder()
	srv.InsightsTrends(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "Internal server error")
}

func TestSummarizeVerdictTrends_Buckets(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	vs := []*persistence.TaskJudgeVerdict{
		{RecordedAt: now, Verdict: "pass"},
		{RecordedAt: now, Verdict: "abstain"},
		{RecordedAt: now, Verdict: "fail"},
		{RecordedAt: now.AddDate(0, 0, -1), Verdict: "pass"},
		{RecordedAt: now.AddDate(0, 0, -30), Verdict: "abstain"}, // outside 14d
	}
	s := summarizeVerdictTrends(vs, now, 14)

	require.Len(t, s.Buckets, 14)
	today := s.Buckets[len(s.Buckets)-1]
	assert.Equal(t, 1, today.Pass)
	assert.Equal(t, 1, today.Fail)
	assert.Equal(t, 1, today.Abstain)
	assert.Equal(t, 3, today.Total)
	assert.Equal(t, 33, today.AbstainPct, "1/3")

	assert.Equal(t, 2, s.TotPass)
	assert.Equal(t, 1, s.TotFail)
	assert.Equal(t, 1, s.TotAbstain)
	assert.Equal(t, 25, s.OverallAbstainPct, "1/(2+1+1)")
	assert.True(t, s.HasData)
}

type stubVerdictRepo struct {
	rows []*persistence.TaskJudgeVerdict
}

func (s *stubVerdictRepo) Record(context.Context, *persistence.TaskJudgeVerdict) error { return nil }
func (s *stubVerdictRepo) GetByTask(context.Context, string) (*persistence.TaskJudgeVerdict, error) {
	return nil, nil
}
func (s *stubVerdictRepo) ListRecent(context.Context, string, int) ([]*persistence.TaskJudgeVerdict, error) {
	return s.rows, nil
}

func TestSummarizeRecoveryTrends_Buckets(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	events := []*persistence.RecoveryEvent{
		{CreatedAt: now},
		{CreatedAt: now},
		{CreatedAt: now.AddDate(0, 0, -2)},
		{CreatedAt: now.AddDate(0, 0, -30)}, // outside window
	}
	s := summarizeRecoveryTrends(events, now, 14)

	require.Len(t, s.Buckets, 14)
	assert.Equal(t, 3, s.Total, "30-days-ago excluded")
	assert.Equal(t, 2, s.Buckets[len(s.Buckets)-1].Count, "two today")
	assert.True(t, s.HasData)
}

func TestSummarizeRecoveryTrends_EmptyHasNoData(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	s := summarizeRecoveryTrends(nil, now, 14)
	assert.False(t, s.HasData)
	assert.Equal(t, 0, s.Total)
}

type stubRecoveryRepo struct{ rows []*persistence.RecoveryEvent }

func (s *stubRecoveryRepo) Record(context.Context, *persistence.RecoveryEvent) error { return nil }
func (s *stubRecoveryRepo) ListRecent(context.Context, string, int) ([]*persistence.RecoveryEvent, error) {
	return s.rows, nil
}

func TestInsightsTrends_RendersRecoverySection(t *testing.T) {
	now := time.Now().UTC()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{CreatedAt: now, Status: persistence.TaskStatusCompleted}}, nil
		},
	}
	recovery := &stubRecoveryRepo{rows: []*persistence.RecoveryEvent{{CreatedAt: now}, {CreatedAt: now}}}
	srv := NewServer(WithTaskRepository(taskRepo), WithRecoveryEventRepository(recovery))

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/trends", nil)
	rec := httptest.NewRecorder()
	srv.InsightsTrends(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Daily recoveries")
}

func TestLayoutSpendTrend_TotalsAndGeometry(t *testing.T) {
	s := layoutSpendTrend([]float64{0, 1.5, 0, 3.0}, []string{"d1", "d2", "d3", "d4"})
	require.Len(t, s.Buckets, 4)
	assert.InDelta(t, 4.5, s.Total, 0.001)
	assert.Equal(t, "$4.50", s.TotalLabel)
	assert.Equal(t, "$3.00", s.Buckets[3].CostLabel)
	assert.True(t, s.HasData)
	// Tallest bar is the max-cost day.
	assert.Greater(t, s.Buckets[3].BarH, s.Buckets[1].BarH)
}

func TestLayoutSpendTrend_ZeroHasNoData(t *testing.T) {
	s := layoutSpendTrend([]float64{0, 0}, []string{"d1", "d2"})
	assert.False(t, s.HasData)
}

func TestInsightsTrends_RendersSpendSection(t *testing.T) {
	now := time.Now().UTC()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{CreatedAt: now, Status: persistence.TaskStatusCompleted}}, nil
		},
	}
	// stubLLMUsageRepo (dashboard_test.go) returns `sum` from SumCost.
	srv := NewServer(WithTaskRepository(taskRepo), WithLLMUsageRepository(&stubLLMUsageRepo{sum: 0.25}))

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/trends", nil)
	rec := httptest.NewRecorder()
	srv.InsightsTrends(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Daily LLM spend")
}

func TestInsightsTrends_RendersVerdictSection(t *testing.T) {
	now := time.Now().UTC()
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{CreatedAt: now, Status: persistence.TaskStatusCompleted}}, nil
		},
	}
	verdicts := &stubVerdictRepo{rows: []*persistence.TaskJudgeVerdict{
		{RecordedAt: now, Verdict: "pass"}, {RecordedAt: now, Verdict: "abstain"},
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithJudgeVerdictRepository(verdicts))

	req := httptest.NewRequest(http.MethodGet, "/ui/insights/trends", nil)
	rec := httptest.NewRecorder()
	srv.InsightsTrends(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Abstain rate")
}
