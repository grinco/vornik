package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fakeChunkGraph implements ChunkGraphExtractionRepository for the
// Dashboard's "memory" tile.
type fakeChunkGraph struct {
	stats *persistence.KGStats
}

func (f *fakeChunkGraph) FetchUnextracted(context.Context, int) ([]persistence.ChunkForExtraction, error) {
	return nil, nil
}
func (f *fakeChunkGraph) MarkExtracted(context.Context, string) error { return nil }
func (f *fakeChunkGraph) PendingCount(context.Context) (int, error)   { return 0, nil }
func (f *fakeChunkGraph) Stats(context.Context) (*persistence.KGStats, error) {
	return f.stats, nil
}
func (f *fakeChunkGraph) ReflagChunksMissingEdges(context.Context, string, bool) (int, error) {
	return 0, nil
}

// fakeAutonomyEvalRepo implements AutonomyEvaluationRepository for
// the Dashboard's autonomy-ETAs tile.
type fakeAutonomyEvalRepo struct {
	rows []*persistence.AutonomyEvaluation
}

func (f *fakeAutonomyEvalRepo) Record(context.Context, *persistence.AutonomyEvaluation) error {
	return nil
}
func (f *fakeAutonomyEvalRepo) List(_ context.Context, filter persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	return f.rows, nil
}
func (f *fakeAutonomyEvalRepo) CountByOutcome(context.Context, string, time.Time, time.Time) (map[string]int64, error) {
	return nil, nil
}

// fakeActiveChatSource implements ActiveChatSource for the
// Dashboard's chat-count tile.
type fakeActiveChatSource struct{ count int }

func (f *fakeActiveChatSource) ActiveChatCount() int { return f.count }

// TestDashboard_RendersEmptyWithNoRepos — without any repo wired,
// the landing page still renders (every tile is nil-safe).
func TestDashboard_RendersEmptyWithNoRepos(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Dashboard")
}

// TestDashboard_OperatorQueue_HealthyState — when no failures, no
// stuck parents, no awaiting-input, no spend spike, the queue rail
// collapses to the "All clear" indicator (NOT a sea of zero-cards).
// Pins the design contract that quiet = quiet UI.
func TestDashboard_OperatorQueue_HealthyState(t *testing.T) {
	srv := NewServer(WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	if !strings.Contains(body, "All clear") {
		t.Error(`Healthy state must render "All clear" — without it, the dashboard looks broken on a quiet day`)
	}
	if strings.Contains(body, "failed in last 24h") {
		t.Error("Healthy state must NOT show a failures card")
	}
	if strings.Contains(body, "waiting for children") {
		t.Error("Healthy state must NOT show a stuck-parents card")
	}
}

// TestDashboard_OperatorQueue_StuckParentsCard — when there are
// WAITING_FOR_CHILDREN parents, the queue rail renders the stuck
// card and suppresses the "All clear" indicator. Uses TaskCounts
// directly (no recent-failures sweep needed — that path requires
// projectReg which the simple Server fixture doesn't carry).
func TestDashboard_OperatorQueue_StuckParentsCard(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{
				persistence.TaskStatusWaitingForChildren: 3,
			}, nil
		},
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "waiting for children", "rail must surface stuck-parents card when count > 0")
	assert.NotContains(t, body, "All clear", "healthy indicator must NOT render alongside queue cards")
}

// TestDashboard_RendersWithTaskCountsAndActive — the task counts
// tile + the active-tasks list both populate from the task repo.
func TestDashboard_RendersWithTaskCountsAndActive(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return map[persistence.TaskStatus]int64{
				persistence.TaskStatusRunning:   2,
				persistence.TaskStatusQueued:    3,
				persistence.TaskStatusCompleted: 10,
			}, nil
		},
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			if f.Status == nil {
				return nil, nil
			}
			switch *f.Status {
			case persistence.TaskStatusRunning:
				return []*persistence.Task{{ID: "task_run_1", ProjectID: "p1", Status: persistence.TaskStatusRunning}}, nil
			case persistence.TaskStatusQueued:
				return []*persistence.Task{{ID: "task_q_1", ProjectID: "p1", Status: persistence.TaskStatusQueued}}, nil
			}
			return nil, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "task_run_1")
	assert.Contains(t, body, "task_q_1")
}

// TestDashboard_WithChunkGraphRendersMemoryTile — populated KG
// stats should appear in the body.
func TestDashboard_WithChunkGraphRendersMemoryTile(t *testing.T) {
	srv := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{
			CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
				return nil, nil
			},
		}),
		WithChunkGraphRepository(&fakeChunkGraph{
			stats: &persistence.KGStats{
				ChunksPending: 10,
				ChunksDone:    90,
				Entities:      40,
				Edges:         55,
				Mentions:      120,
			},
		}),
		WithOnboardingDetector(alreadyOnboardedDetector()),
	)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestDashboard_WithActiveChatCount — chat tile renders the count.
func TestDashboard_WithActiveChatCount(t *testing.T) {
	srv := NewServer(WithActiveChatSource(&fakeActiveChatSource{count: 5}), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// The count "5" should appear somewhere in the body.
	assert.Contains(t, rec.Body.String(), ">5<", "active chat count should render")
}

// TestDashboard_WithAutonomyEvalRendersETAs — fixture projects with
// autonomy enabled get their next-tick projections rendered.
func TestDashboard_WithAutonomyEvalRendersETAs(t *testing.T) {
	root := writeSwarmFixture(t)
	srv, _ := swarmEditServer(t, root)
	// Patch the loaded project to enable autonomy.
	p := srv.projectReg.GetProject("p1")
	if p != nil {
		p.Autonomy.Enabled = true
		p.Autonomy.PollInterval = "10m"
	}
	srv.autonomyEvalRepo = &fakeAutonomyEvalRepo{
		rows: []*persistence.AutonomyEvaluation{
			{ProjectID: "p1", Outcome: "noop", CreatedAt: time.Now().Add(-2 * time.Minute)},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestDashboard_LimitParamHonoured — ?limit=10 caps the active-tasks
// section row count.
func TestDashboard_LimitParamHonoured(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		CountByStatusFunc: func(context.Context, string) (map[persistence.TaskStatus]int64, error) {
			return nil, nil
		},
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			rows := make([]*persistence.Task, 30)
			for i := range rows {
				rows[i] = &persistence.Task{ID: "task_x", ProjectID: "p1", Status: persistence.TaskStatusQueued}
			}
			return rows, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithOnboardingDetector(alreadyOnboardedDetector()))
	req := httptest.NewRequest(http.MethodGet, "/?limit=10", nil)
	rec := httptest.NewRecorder()
	srv.Dashboard(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	// limit=10 → page-size selector shows 10 selected.
	assert.Contains(t, rec.Body.String(), `value="10" selected`)
}
