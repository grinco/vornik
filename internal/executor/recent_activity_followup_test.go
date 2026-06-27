package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubTradingOrderRepoY is a minimal TradingOrderRepository for the
// recent-activity block test. Record / Count are panics — they're
// not exercised by buildRecentActivityBlock. List honours the
// configured `rows` / `err`.
type stubTradingOrderRepoY struct {
	mu       sync.Mutex
	rows     []*persistence.TradingOrder
	listErr  error
	lastFilt persistence.TradingOrderFilter
}

func (s *stubTradingOrderRepoY) Record(_ context.Context, _ *persistence.TradingOrder) error {
	panic("not implemented")
}
func (s *stubTradingOrderRepoY) Count(_ context.Context, _ persistence.TradingOrderFilter) (int64, error) {
	panic("not implemented")
}
func (s *stubTradingOrderRepoY) List(_ context.Context, f persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFilt = f
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows, nil
}

// TestBuildRecentActivityBlock_NoRepo — when the executor is built
// without the trading repo (the normal deployment for non-trading
// projects), the helper must short-circuit to empty string. A
// non-empty return would pollute the strategist prompt of every
// project on the box.
func TestBuildRecentActivityBlock_NoRepo(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))
}

// TestBuildRecentActivityBlock_EmptyProjectID — guard prevents
// the helper from joining all-projects rows into one prompt.
func TestBuildRecentActivityBlock_EmptyProject(t *testing.T) {
	e := &Executor{
		tradingOrderRepo: &stubTradingOrderRepoY{rows: []*persistence.TradingOrder{{Symbol: "AAPL"}}},
		logger:           zerolog.Nop(),
	}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), ""))
}

// TestBuildRecentActivityBlock_RepoError — the repo blip
// short-circuits to empty so the strategist still runs (this is
// best-effort context, not a hard dependency).
func TestBuildRecentActivityBlock_RepoError(t *testing.T) {
	e := &Executor{
		tradingOrderRepo: &stubTradingOrderRepoY{listErr: errors.New("db blip")},
		logger:           zerolog.Nop(),
	}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))
}

// TestBuildRecentActivityBlock_NoRows — fresh project with no
// recent activity returns empty so the prompt doesn't include
// an empty section header.
func TestBuildRecentActivityBlock_NoRows(t *testing.T) {
	e := &Executor{
		tradingOrderRepo: &stubTradingOrderRepoY{rows: nil},
		logger:           zerolog.Nop(),
	}
	assert.Equal(t, "", e.buildRecentActivityBlock(context.Background(), "p1"))
}

// TestBuildRecentActivityBlock_RendersRows — the standard
// rendering path. Pin the exact format: symbol, qty, status, and
// optional reason all surface; the 24h window is captured in the
// since filter; PageSize=20 caps the output for busy days.
func TestBuildRecentActivityBlock_RendersRows(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Hour)
	limit := 173.25
	stop := 170.00
	repo := &stubTradingOrderRepoY{
		rows: []*persistence.TradingOrder{
			{
				Symbol:           "AAPL",
				Action:           "buy",
				Qty:              5.0,
				LimitPrice:       &limit,
				StopPrice:        &stop,
				Status:           "filled",
				LastStatusReason: "ok",
				SubmittedAt:      now,
			},
			{
				Symbol:      "MSFT",
				Action:      "sell",
				Qty:         2.0,
				Status:      "cancelled",
				SubmittedAt: now.Add(-time.Hour),
			},
		},
	}
	e := &Executor{tradingOrderRepo: repo, logger: zerolog.Nop()}
	out := e.buildRecentActivityBlock(context.Background(), "trading-proj")

	require.NotEmpty(t, out, "rendered block must be non-empty when repo returns rows")
	assert.Contains(t, out, "AAPL", "first row's symbol must appear")
	assert.Contains(t, out, "buy", "first row's action must appear")
	assert.Contains(t, out, "filled", "status must appear")
	assert.Contains(t, out, "$173.25", "limit price must render with 2 decimals + $ prefix")
	assert.Contains(t, out, "stop $170.00", "stop price must surface alongside limit")
	assert.Contains(t, out, "MSFT", "second row's symbol must appear")
	assert.Contains(t, out, "cooldown", "guidance preamble must mention cooldown")

	// since filter must be ~24h ago — within a wide tolerance to
	// keep the test fast and time-skew tolerant.
	require.NotNil(t, repo.lastFilt.Since)
	delta := time.Since(*repo.lastFilt.Since)
	assert.InDelta(t, (24 * time.Hour).Seconds(), delta.Seconds(), 60.0,
		"since filter must be 24h ago (within 60s tolerance)")
	assert.Equal(t, 20, repo.lastFilt.PageSize, "page size cap must be 20")
}

// TestBuildRecentActivityBlock_SkipsNilRows — the loop guards
// against nil entries (legacy repos that may emit them); the
// helper continues past the nil rather than panicking.
func TestBuildRecentActivityBlock_SkipsNilRows(t *testing.T) {
	now := time.Now().UTC()
	repo := &stubTradingOrderRepoY{
		rows: []*persistence.TradingOrder{
			nil, // unexpected nil row
			{Symbol: "TSLA", Action: "buy", Qty: 1, Status: "submitted", SubmittedAt: now},
		},
	}
	e := &Executor{tradingOrderRepo: repo, logger: zerolog.Nop()}
	out := e.buildRecentActivityBlock(context.Background(), "trading-proj")
	assert.Contains(t, out, "TSLA",
		"helper must skip nil row and continue rendering the real one")
}

// stubMemoryIndexerY captures IngestText calls so tests can assert
// what was sent into project memory.
type stubMemoryIndexerY struct {
	mu    sync.Mutex
	calls []memCall
	err   error
}

type memCall struct {
	projectID, taskID, artifactID, source, content string
}

func (s *stubMemoryIndexerY) IngestText(_ context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, memCall{projectID, taskID, artifactID, sourceName, content})
	return s.err
}

// PatchScopeByArtifact — migration 75/76. No-op stub; the tests
// that use stubMemoryIndexerY don't exercise the scope path.
func (s *stubMemoryIndexerY) PatchScopeByArtifact(_ context.Context, _, _, _ string) error {
	return nil
}

// TestIngestTradingActivity_Disabled — nil memoryIndexer and
// empty result both short-circuit. These are the two
// "non-trading deployment" code paths every executor runs through.
func TestIngestTradingActivity_NilOrEmpty(t *testing.T) {
	// nil indexer
	e := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.ingestTradingActivity(context.Background(),
			&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, []byte(`{"placed":[]}`))
	})

	// indexer wired but result empty — also a no-op.
	mi := &stubMemoryIndexerY{}
	e2 := &Executor{memoryIndexer: mi, logger: zerolog.Nop()}
	e2.ingestTradingActivity(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"}, nil)
	assert.Empty(t, mi.calls)
}

// TestIngestTradingActivity_NonTradingShape — the helper only
// fires when the result carries placed/skipped/fills_observed
// arrays. A non-trading result shape (e.g. a research summary)
// must NOT generate a trading-activity memory chunk; otherwise
// every workflow would pollute the project memory with empty
// stamps.
func TestIngestTradingActivity_NonTradingShape(t *testing.T) {
	mi := &stubMemoryIndexerY{}
	e := &Executor{memoryIndexer: mi, logger: zerolog.Nop()}
	e.ingestTradingActivity(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
		[]byte(`{"placed":[],"skipped":[],"fills_observed":[]}`))
	assert.Empty(t, mi.calls,
		"empty trading arrays must NOT produce an ingest call")
}

// TestIngestTradingActivity_GarbledResult — invalid JSON must
// short-circuit (best-effort: a parse failure is silent).
func TestIngestTradingActivity_GarbledJSON(t *testing.T) {
	mi := &stubMemoryIndexerY{}
	e := &Executor{memoryIndexer: mi, logger: zerolog.Nop()}
	e.ingestTradingActivity(context.Background(),
		&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"},
		[]byte("not json {{{"))
	assert.Empty(t, mi.calls)
}

// TestIngestTradingActivity_PlacedFillsSkipped — the full
// rendering path: each section (Fills observed, Orders placed,
// Skipped / cancelled) appears with the right rows. Pin the
// markdown structure since the strategist's downstream
// memory_search relies on the section headings to discover
// these chunks ("ibkr-trader recent fills" lookup).
func TestIngestTradingActivity_FullShape(t *testing.T) {
	mi := &stubMemoryIndexerY{}
	e := &Executor{memoryIndexer: mi, logger: zerolog.Nop()}

	body := []byte(`{
		"placed":[{"symbol":"AAPL","broker_order_id":"o1","status":"submitted"}],
		"skipped":[{"symbol":"TSLA","reason":"cap_refused","detail":"daily cap hit","cancel_reason":"market_moved","cancel_detail":">2% gap"}],
		"fills_observed":[{"symbol":"MSFT","qty":2,"price":312.5}]
	}`)
	e.ingestTradingActivity(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "proj"},
		&persistence.Execution{ID: "exec-1"}, body)

	require.Len(t, mi.calls, 1)
	c := mi.calls[0]
	assert.Equal(t, "proj", c.projectID)
	assert.Equal(t, "t", c.taskID)
	assert.Contains(t, c.source, "trading-activity-exec-1.md",
		"sourceName must embed execution ID for chunk uniqueness")
	assert.Contains(t, c.content, "## Fills observed")
	assert.Contains(t, c.content, "MSFT: 2.0000 @ $312.50")
	assert.Contains(t, c.content, "## Orders placed")
	assert.Contains(t, c.content, "AAPL: status=submitted broker_id=o1")
	assert.Contains(t, c.content, "## Skipped / cancelled")
	assert.Contains(t, c.content, "TSLA: cap_refused")
	assert.Contains(t, c.content, "daily cap hit")
	assert.Contains(t, c.content, "cancel_reason=market_moved")
}

// TestIngestTradingActivity_IndexerError_Logged — the indexer
// returns an error; the helper logs and continues (best-effort).
func TestIngestTradingActivity_IndexerError(t *testing.T) {
	mi := &stubMemoryIndexerY{err: errors.New("memory store full")}
	e := &Executor{memoryIndexer: mi, logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.ingestTradingActivity(context.Background(),
			&persistence.Task{ID: "t", ProjectID: "p"},
			&persistence.Execution{ID: "x"},
			[]byte(`{"placed":[{"symbol":"A","status":"ok"}]}`))
	})
}
