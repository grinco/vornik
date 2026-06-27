package budget

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubRepo returns canned daily/monthly totals. Daily is the sum for any
// since within the last 24h; monthly otherwise. Good enough for policy tests.
type stubRepo struct {
	daily   float64
	monthly float64
	err     error
}

func (s *stubRepo) SumCostByProject(_ context.Context, _ string, since, _ time.Time) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	// Heuristic fixture: if `since` lands on the 1st of a month at 00:00 UTC,
	// treat the call as a monthly sum; otherwise treat it as a daily sum.
	// Check.budget converts day/month starts back to UTC, so this matches
	// the real call shape for both non-TZ and TZ-configured projects
	// (TZ-aware tests use a separate tzSpyRepo).
	if since.Day() == 1 {
		return s.monthly, nil
	}
	return s.daily, nil
}

func TestCheck_NoBudgetConfigured(t *testing.T) {
	p := &registry.Project{ID: "p"}
	d, err := Check(context.Background(), &stubRepo{daily: 100, monthly: 500}, p, time.Now())
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.False(t, d.SoftBreached)
}

func TestCheck_NilProjectSafe(t *testing.T) {
	d, err := Check(context.Background(), &stubRepo{daily: 100}, nil, time.Now())
	require.NoError(t, err)
	assert.False(t, d.Blocked)
}

func TestCheck_NilRepoSafe(t *testing.T) {
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 10}}
	d, err := Check(context.Background(), nil, p, time.Now())
	require.NoError(t, err)
	assert.False(t, d.Blocked)
}

func TestCheck_DailyHardCapBlocks(t *testing.T) {
	// Pick a non-first day to route through daily lookup.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{
		ID:     "p",
		Budget: registry.ProjectBudget{DailyHardUSD: 5},
	}
	d, err := Check(context.Background(), &stubRepo{daily: 6.50, monthly: 20}, p, now)
	require.NoError(t, err)
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "daily budget exceeded")
	assert.InDelta(t, 6.50, d.DailyUSD, 1e-9)
}

func TestCheck_MonthlyHardCapBlocks(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{
		ID:     "p",
		Budget: registry.ProjectBudget{MonthlyHardUSD: 20},
	}
	d, err := Check(context.Background(), &stubRepo{daily: 2, monthly: 20.50}, p, now)
	require.NoError(t, err)
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "monthly budget exceeded")
}

func TestCheck_DailySoftCapWarns(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{
		ID:     "p",
		Budget: registry.ProjectBudget{DailySoftUSD: 5, DailyHardUSD: 20},
	}
	d, err := Check(context.Background(), &stubRepo{daily: 7, monthly: 10}, p, now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.True(t, d.SoftBreached)
}

func TestCheck_HardBeatsSoft(t *testing.T) {
	// Both soft + hard would trigger but hard wins.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{
		ID:     "p",
		Budget: registry.ProjectBudget{DailySoftUSD: 5, DailyHardUSD: 10},
	}
	d, err := Check(context.Background(), &stubRepo{daily: 11, monthly: 11}, p, now)
	require.NoError(t, err)
	assert.True(t, d.Blocked)
	assert.False(t, d.SoftBreached)
}

// tzSpyRepo records the since values each SumCostByProject call receives
// so the test can assert the day/month boundaries were computed in the
// configured timezone.
type tzSpyRepo struct {
	seen []time.Time
}

func (r *tzSpyRepo) SumCostByProject(_ context.Context, _ string, since, _ time.Time) (float64, error) {
	r.seen = append(r.seen, since)
	return 0, nil
}

func TestCheck_TimezoneAffectsDayBoundary(t *testing.T) {
	// Prague is UTC+2 in summer, so local midnight = 22:00 UTC the day before.
	// Caller time: 2026-06-15 10:00 UTC = 2026-06-15 12:00 Prague.
	// Expected dayStart = 2026-06-15 00:00 Prague = 2026-06-14 22:00 UTC.
	p := &registry.Project{
		ID: "p",
		Budget: registry.ProjectBudget{
			DailyHardUSD: 100, // present so Check actually queries
			Timezone:     "Europe/Prague",
		},
	}
	repo := &tzSpyRepo{}
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	_, err := Check(context.Background(), repo, p, now)
	require.NoError(t, err)
	require.Len(t, repo.seen, 2) // daily sum + monthly sum

	dayStart := repo.seen[0]
	// 2026-06-14 22:00 UTC in Unix seconds, not 2026-06-15 00:00 UTC.
	expected := time.Date(2026, 6, 14, 22, 0, 0, 0, time.UTC)
	require.Equal(t, expected, dayStart, "daily window should start at local midnight converted to UTC")

	monthStart := repo.seen[1]
	// 2026-06-01 00:00 Prague = 2026-05-31 22:00 UTC
	expectedMonth := time.Date(2026, 5, 31, 22, 0, 0, 0, time.UTC)
	require.Equal(t, expectedMonth, monthStart)
}

func TestCheck_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	p := &registry.Project{
		ID: "p",
		Budget: registry.ProjectBudget{
			DailyHardUSD: 100,
			Timezone:     "Not/A_Real_Zone",
		},
	}
	repo := &tzSpyRepo{}
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	_, err := Check(context.Background(), repo, p, now)
	require.NoError(t, err)
	// Invalid zone → UTC → dayStart is 2026-06-15 00:00 UTC.
	expected := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	require.Equal(t, expected, repo.seen[0])
}

func TestCheck_Under_AllCaps(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{
		ID: "p",
		Budget: registry.ProjectBudget{
			DailySoftUSD: 5, DailyHardUSD: 10,
			MonthlySoftUSD: 50, MonthlyHardUSD: 100,
		},
	}
	d, err := Check(context.Background(), &stubRepo{daily: 2, monthly: 20}, p, now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.False(t, d.SoftBreached)
	assert.InDelta(t, 2.0, d.DailyUSD, 1e-9)
	assert.InDelta(t, 20.0, d.MonthlyUSD, 1e-9)
}

func (s *stubRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (r *tzSpyRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

// --- Reserve (trading-hardening §1) -----------------------------------

// fakeReservationRepo records calls and returns a canned reserved sum and
// block decision so Reserve's orchestration can be exercised without a DB.
type fakeReservationRepo struct {
	reservedSum   float64 // returned as ReservedUSD
	lastReq       persistence.ReserveRequest
	reserveCalled bool
	settleCalled  bool
	forceErr      error
}

func (f *fakeReservationRepo) Reserve(_ context.Context, req persistence.ReserveRequest) (persistence.ReserveResult, error) {
	f.reserveCalled = true
	f.lastReq = req
	if f.forceErr != nil {
		return persistence.ReserveResult{}, f.forceErr
	}
	out := persistence.ReserveResult{ReservedUSD: f.reservedSum}
	if req.DailyHardUSD > 0 && req.DailyCommittedUSD+f.reservedSum+req.EstimateUSD > req.DailyHardUSD {
		out.Blocked, out.Period = true, "daily"
		return out, nil
	}
	if req.MonthlyHardUSD > 0 && req.MonthlyCommittedUSD+f.reservedSum+req.EstimateUSD > req.MonthlyHardUSD {
		out.Blocked, out.Period = true, "monthly"
		return out, nil
	}
	out.Reserved = true
	return out, nil
}

func (f *fakeReservationRepo) SettleByTask(_ context.Context, _ string, _ time.Time) (int64, error) {
	f.settleCalled = true
	return 0, nil
}

func TestReserve_NoHardCap_NoOp(t *testing.T) {
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailySoftUSD: 5}}
	f := &fakeReservationRepo{}
	d, err := Reserve(context.Background(), f, &stubRepo{daily: 1}, p, "task-1", time.Now())
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.False(t, f.reserveCalled, "no hard cap → must not touch the ledger")
}

func TestReserve_InsertsWhenUnderCap(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 100}}
	f := &fakeReservationRepo{reservedSum: 10}
	d, err := Reserve(context.Background(), f, &stubRepo{daily: 20, monthly: 20}, p, "task-1", now)
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	assert.True(t, f.reserveCalled)
	assert.Equal(t, "task-1", f.lastReq.TaskID)
	// Default estimate applied when the project sets none.
	assert.InDelta(t, DefaultReservationEstimateUSD, f.lastReq.EstimateUSD, 1e-9)
}

func TestReserve_BlocksWhenCommittedPlusReservedPlusEstimateExceedsCap(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	// committed 90 + reserved 8 + estimate 5 = 103 > 100 daily hard cap.
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 100, ReservationEstimateUSD: 5}}
	f := &fakeReservationRepo{reservedSum: 8}
	d, err := Reserve(context.Background(), f, &stubRepo{daily: 90, monthly: 90}, p, "task-1", now)
	require.NoError(t, err)
	assert.True(t, d.Blocked)
	assert.Contains(t, d.Reason, "daily budget would be exceeded")
}

func TestReserve_CustomEstimateThreaded(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 100, ReservationEstimateUSD: 7.5}}
	f := &fakeReservationRepo{}
	_, err := Reserve(context.Background(), f, &stubRepo{daily: 1, monthly: 1}, p, "task-1", now)
	require.NoError(t, err)
	assert.InDelta(t, 7.5, f.lastReq.EstimateUSD, 1e-9)
}

func TestReserve_NilSafe(t *testing.T) {
	p := &registry.Project{ID: "p", Budget: registry.ProjectBudget{DailyHardUSD: 10}}
	d, err := Reserve(context.Background(), nil, &stubRepo{daily: 1}, p, "task-1", time.Now())
	require.NoError(t, err)
	assert.False(t, d.Blocked)
	// Empty taskID is a no-op too.
	d2, err2 := Reserve(context.Background(), &fakeReservationRepo{}, &stubRepo{daily: 1}, p, "", time.Now())
	require.NoError(t, err2)
	assert.False(t, d2.Blocked)
}
