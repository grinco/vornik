package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// stubTradingOrderRepo is the minimal TradingOrderRepository surface
// the recent-activity block needs. Records the filter so tests can
// assert on the windowing contract too.
type stubTradingOrderRepo struct {
	rows     []*persistence.TradingOrder
	err      error
	listCall persistence.TradingOrderFilter
	called   int
}

func (s *stubTradingOrderRepo) Record(_ context.Context, _ *persistence.TradingOrder) error {
	return nil
}

func (s *stubTradingOrderRepo) List(_ context.Context, filter persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	s.called++
	s.listCall = filter
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *stubTradingOrderRepo) Count(_ context.Context, _ persistence.TradingOrderFilter) (int64, error) {
	return int64(len(s.rows)), nil
}

func TestBuildRecentActivityBlock_NoRepoOrEmptyProjectIDReturnsEmpty(t *testing.T) {
	// Nil repo → empty even with a project ID.
	e := &Executor{logger: zerolog.Nop()}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))

	// Empty project ID → empty even with a repo.
	repo := &stubTradingOrderRepo{}
	e.tradingOrderRepo = repo
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), ""))
	assert.Equal(t, 0, repo.called)
}

func TestBuildRecentActivityBlock_RepoErrorReturnsEmpty(t *testing.T) {
	repo := &stubTradingOrderRepo{err: errors.New("db down")}
	e := &Executor{logger: zerolog.Nop(), tradingOrderRepo: repo}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))
}

func TestBuildRecentActivityBlock_EmptyRowsReturnsEmpty(t *testing.T) {
	repo := &stubTradingOrderRepo{rows: nil}
	e := &Executor{logger: zerolog.Nop(), tradingOrderRepo: repo}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))
}

func TestBuildRecentActivityBlock_RendersRowsWithOptionalFields(t *testing.T) {
	limitPx := 42.50
	stopPx := 41.00
	zero := 0.0
	rows := []*persistence.TradingOrder{
		{
			Symbol:           "AAPL",
			Action:           "BUY",
			Qty:              10.0,
			Status:           "filled",
			SubmittedAt:      time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC),
			LimitPrice:       &limitPx,
			StopPrice:        &stopPx,
			LastStatusReason: "filled at open",
		},
		{
			Symbol:      "MSFT",
			Action:      "SELL",
			Qty:         5.5,
			Status:      "submitted",
			SubmittedAt: time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC),
			// No optional fields — render compact line.
		},
		nil, // nil row inside the slice is skipped.
		{
			Symbol:      "ZERO",
			Action:      "BUY",
			Qty:         1.0,
			Status:      "rejected",
			SubmittedAt: time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC),
			// Zero LimitPrice → no @ price formatting.
			LimitPrice: &zero,
			StopPrice:  &zero,
		},
	}
	repo := &stubTradingOrderRepo{rows: rows}
	e := &Executor{logger: zerolog.Nop(), tradingOrderRepo: repo}
	out := e.buildRecentActivityBlock(context.Background(), "proj-1")

	require.NotEmpty(t, out)
	// Header line is always present.
	assert.True(t, strings.HasPrefix(out, "Last 24h of broker activity"))
	// Each non-nil row appears once.
	assert.Contains(t, out, "AAPL BUY")
	assert.Contains(t, out, "@ $42.50")
	assert.Contains(t, out, "stop $41.00")
	assert.Contains(t, out, "· filled at open")
	assert.Contains(t, out, "MSFT SELL")
	assert.Contains(t, out, "status=submitted")
	assert.Contains(t, out, "ZERO BUY")
	// Zero-priced row gets no @ / stop in output.
	assert.NotContains(t, out, "ZERO BUY 1.0000 @")

	// Repo filter is windowed to ~24h.
	require.Equal(t, 1, repo.called)
	require.NotNil(t, repo.listCall.Since)
	assert.WithinDuration(t, time.Now().UTC().Add(-24*time.Hour), *repo.listCall.Since, 5*time.Second)
	require.NotNil(t, repo.listCall.ProjectID)
	assert.Equal(t, "proj-1", *repo.listCall.ProjectID)
	assert.Equal(t, 20, repo.listCall.PageSize)
}
