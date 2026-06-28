package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestSparklinePoints(t *testing.T) {
	cases := []struct {
		name      string
		in        []float64
		wantLine  string
		wantEmpty bool
	}{
		{"empty", nil, "", true},
		{"single point", []float64{1}, "", true},
		{"all zeros", []float64{0, 0, 0}, "", true},
		{"two points", []float64{1, 2}, "0.0,12.0 100.0,0.0", false},
		{"flat non-zero", []float64{5, 5, 5}, "0.0,0.0 50.0,0.0 100.0,0.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, filled := sparklinePoints(tc.in, 100, 24)
			if tc.wantEmpty {
				if line != "" || filled != "" {
					t.Errorf("expected empty for %v, got line=%q filled=%q", tc.in, line, filled)
				}
				return
			}
			if line != tc.wantLine {
				t.Errorf("line = %q, want %q", line, tc.wantLine)
			}
			// Filled polyline must close along the baseline.
			if !strings.HasSuffix(filled, " 100,24 0,24") {
				t.Errorf("filled missing closing baseline points: %q", filled)
			}
		})
	}
}

// stubLLMUsageRepo is a minimal stand-in for the dashboard render test.
// Only the methods Dashboard touches are implemented; the rest panic
// loudly so a regression that calls a new method shows up clearly.
type stubLLMUsageRepo struct {
	rows  []*persistence.TaskLLMUsage
	sum   float64
	roles []persistence.RoleModelSpend
}

func (s *stubLLMUsageRepo) Record(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *stubLLMUsageRepo) Upsert(context.Context, *persistence.TaskLLMUsage) error { return nil }
func (s *stubLLMUsageRepo) List(context.Context, persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return s.rows, nil
}
func (s *stubLLMUsageRepo) SumCost(context.Context, time.Time, time.Time) (float64, error) {
	return s.sum, nil
}
func (s *stubLLMUsageRepo) SumCostByProject(context.Context, string, time.Time, time.Time) (float64, error) {
	return 0, nil
}
func (s *stubLLMUsageRepo) AggregateByRoleModel(context.Context, time.Time, time.Time, int, string) ([]persistence.RoleModelSpend, error) {
	return s.roles, nil
}

// Aggregations added by the spend deep-dive. Stubs return nil so
// the dashboard test stays focused on the headline + leaderboard
// path; the dedicated spend tests cover these surfaces.
func (s *stubLLMUsageRepo) AggregateByProject(context.Context, time.Time, time.Time, int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) AggregateBySource(context.Context, time.Time, time.Time, string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TimeSeriesByDay(context.Context, time.Time, time.Time, string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TopTasks(context.Context, time.Time, time.Time, int, string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TaskCostBreakdown(context.Context, string) ([]persistence.StepSpend, error) {
	return nil, nil
}

// renderDashboardBody builds a default server with an already-onboarded
// detector (so the dashboard doesn't redirect to /ui/setup), GETs /ui/,
// asserts a 200, and returns the rendered body. Shared by the dashboard
// render tests so the NewServer + ServeHTTP boilerplate lives in one place.
func renderDashboardBody(t *testing.T, opts ...ServerOption) string {
	t.Helper()
	all := append([]ServerOption{WithOnboardingDetector(alreadyOnboardedDetector())}, opts...)
	srv := NewServer(all...)
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d, body: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestDashboardUsesPrimitivesAndSemanticTokens guards the Phase-0 re-tier:
// the stat counts render through the statStrip primitive (positive-assert its
// grid class in the body), and the template carries no legacy gray-*/dark-*
// tokens (source-scoped negative-assert).
func TestDashboardUsesPrimitivesAndSemanticTokens(t *testing.T) {
	body := renderDashboardBody(t)
	const stripGrid = "grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-4 text-xs"
	if !strings.Contains(body, stripGrid) {
		t.Errorf("dashboard body missing statStrip grid %q:\n%s", stripGrid, body)
	}
	assertNoLegacyTokens(t, "dashboard.html")
}

// TestDashboardRendersWithSpend reproduces the regression where the
// dashboard 500'd whenever bucketHourly returned a non-zero series — the
// sparkline template branch hit a `sub <int> <float>` and crashed mid-
// render. With the polyline pre-computed in Go, this test guards against
// that returning.
func TestDashboardRendersWithSpend(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubLLMUsageRepo{
		sum: 12.34,
		rows: []*persistence.TaskLLMUsage{
			{CostUSD: 0.50, RecordedAt: now.Add(-30 * time.Minute)},
			{CostUSD: 1.25, RecordedAt: now.Add(-3 * time.Hour)},
			{CostUSD: 0.10, RecordedAt: now.Add(-22 * time.Hour)},
		},
	}
	srv := NewServer(WithLLMUsageRepository(repo), WithOnboardingDetector(alreadyOnboardedDetector()))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// The sparkline must actually appear — non-zero series should produce
	// a polyline element. If the math regressed, the template would error
	// out and the body would be truncated short of this marker.
	if !strings.Contains(body, "<polyline") {
		t.Errorf("dashboard body missing sparkline polyline (template render likely failed):\n%s", body)
	}
	// The headline figure must be there too.
	if !strings.Contains(body, "$12.34") {
		t.Errorf("dashboard body missing 24h spend headline $12.34:\n%s", body)
	}
}

// stubCacheStatsSource is the minimal fake satisfying
// ResponseCacheStatsSource for the dashboard tile tests below.
type stubCacheStatsSource struct {
	stats ResponseCacheStats
	err   error
}

func (s *stubCacheStatsSource) CacheStats(_ context.Context) (ResponseCacheStats, error) {
	return s.stats, s.err
}

func TestDashboard_CacheTileVisibleWhenHitsExist(t *testing.T) {
	cache := &stubCacheStatsSource{
		stats: ResponseCacheStats{
			RowCount: 5, TotalHits: 42, TotalSavingsUSD: 1.23,
		},
	}
	srv := NewServer(WithResponseCacheStatsSource(cache), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "LLM cache savings") {
		t.Errorf("expected cache tile heading, body:\n%s", body)
	}
	if !strings.Contains(body, "$1.23") {
		t.Errorf("expected savings number in body:\n%s", body)
	}
}

func TestDashboard_CacheTileHiddenWhenNoHits(t *testing.T) {
	cache := &stubCacheStatsSource{
		stats: ResponseCacheStats{RowCount: 3, TotalHits: 0},
	}
	srv := NewServer(WithResponseCacheStatsSource(cache), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "LLM cache savings") {
		t.Error("cache tile must hide when no hits exist")
	}
}

func TestDashboard_CacheTileHiddenWhenUnwired(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard returned %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "LLM cache savings") {
		t.Error("cache tile must hide when responseCacheStats unwired")
	}
}

func (s *stubLLMUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (s *stubLLMUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
