package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// replayingFillRepo serves a fixed list back, recording the filter
// it was called with so the test can assert the handler queried for
// the right project + UTC-day window.
type replayingFillRepo struct {
	rows         []*persistence.TradingFill
	lastFilter   persistence.TradingFillFilter
	receivedList bool
}

func (r *replayingFillRepo) Record(context.Context, *persistence.TradingFill) error {
	return nil
}

func (r *replayingFillRepo) List(_ context.Context, f persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	r.lastFilter = f
	r.receivedList = true
	return r.rows, nil
}

func (r *replayingFillRepo) SumVolume(context.Context, persistence.TradingFillFilter) (float64, error) {
	return 0, nil
}

func (r *replayingFillRepo) MaxFilledAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (r *replayingFillRepo) PatchCommission(context.Context, string, float64) error {
	return nil
}

func (r *replayingFillRepo) RecordShadow(context.Context, *persistence.TradingFill) error {
	return nil
}

func (r *replayingFillRepo) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

type replayingOrderRepo struct {
	rows       []*persistence.TradingOrder
	lastFilter persistence.TradingOrderFilter
}

func (r *replayingOrderRepo) Record(context.Context, *persistence.TradingOrder) error {
	return nil
}

func (r *replayingOrderRepo) List(_ context.Context, f persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	r.lastFilter = f
	return r.rows, nil
}

func (r *replayingOrderRepo) Count(context.Context, persistence.TradingOrderFilter) (int64, error) {
	return 0, nil
}

func newServerWithRepos(fills *replayingFillRepo, orders *replayingOrderRepo) *Server {
	return NewServer(
		WithLogger(zerolog.Nop()),
		WithTradingFillRepository(fills),
		WithTradingOrderRepository(orders),
	)
}

// replayingSnapshotRepo serves a fixed equity time-series back for
// the T5 breaker-state recovery test.
type replayingSnapshotRepo struct {
	rows []*persistence.TradingPositionsSnapshot
}

func (r *replayingSnapshotRepo) Record(context.Context, *persistence.TradingPositionsSnapshot) error {
	return nil
}

func (r *replayingSnapshotRepo) ListSince(context.Context, string, time.Time, int) ([]*persistence.TradingPositionsSnapshot, error) {
	return r.rows, nil
}

// Audit T5 + T6: the replay response must carry still-working orders
// (for the double-submit guard) and the breaker HWM + day-open
// baseline (derived from the equity snapshots) so a broker restart
// neither double-submits nor launders a drawdown.
func TestStateReplay_T5BreakerStateAndT6WorkingOrders(t *testing.T) {
	now := time.Now().UTC()
	orders := &replayingOrderRepo{
		rows: []*persistence.TradingOrder{
			{ // terminal — excluded from working_orders
				ID: "ord-filled", ProjectID: "ibkr-trader", IdempotencyKey: "k-filled",
				Symbol: "TSLA", Action: "BUY", OrderType: "MKT", Qty: 2,
				Status: "filled", SubmittedAt: now.Add(-40 * time.Minute),
			},
			{ // still working — MUST appear in working_orders
				ID: "ord-working", ProjectID: "ibkr-trader", IdempotencyKey: "k-working",
				Symbol: "NVDA", Action: "BUY", OrderType: "LMT", Qty: 3,
				Status: "submitted", SubmittedAt: now.Add(-10 * time.Minute),
			},
		},
	}
	snaps := &replayingSnapshotRepo{
		rows: []*persistence.TradingPositionsSnapshot{
			{ProjectID: "ibkr-trader", EquityUSD: 10000, RecordedAt: now.Add(-6 * time.Hour)}, // day open
			{ProjectID: "ibkr-trader", EquityUSD: 10500, RecordedAt: now.Add(-3 * time.Hour)}, // peak
			{ProjectID: "ibkr-trader", EquityUSD: 9800, RecordedAt: now.Add(-1 * time.Hour)},
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTradingFillRepository(&replayingFillRepo{}),
		WithTradingOrderRepository(orders),
		WithTradingPositionsSnapshotRepository(snaps),
	)

	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got stateReplayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}

	// T6: only the working order, with its idempotency key.
	if len(got.WorkingOrders) != 1 {
		t.Fatalf("expected 1 working order, got %d: %+v", len(got.WorkingOrders), got.WorkingOrders)
	}
	if got.WorkingOrders[0].ClientTag != "ord-working" || got.WorkingOrders[0].IdempotencyKey != "k-working" {
		t.Fatalf("working order not propagated with its idempotency key: %+v", got.WorkingOrders[0])
	}

	// T5: HWM = peak, baseline = day-open.
	if got.HWMUSD != 10500 {
		t.Fatalf("HWM must be the peak equity (10500), got %v", got.HWMUSD)
	}
	if got.SessionStartEquityUSD != 10000 {
		t.Fatalf("session baseline must be the day-open equity (10000), got %v", got.SessionStartEquityUSD)
	}
}

// TestStateReplay_ReturnsFillsAndOrders — the headline contract: the
// handler returns today's UTC fills + last-hour orders so the broker's
// safety envelope can rebuild dayTurnover and orderTimes after a recreate.
func TestStateReplay_ReturnsFillsAndOrders(t *testing.T) {
	now := time.Now().UTC()
	lp := 434.10
	fills := &replayingFillRepo{
		rows: []*persistence.TradingFill{
			{
				ID:        "fill-1",
				OrderID:   "ord-1",
				ProjectID: "ibkr-trader",
				Symbol:    "TSLA",
				Qty:       2,
				Price:     434.10,
				FilledAt:  now.Add(-30 * time.Minute),
			},
			{
				ID:        "fill-2",
				OrderID:   "ord-2",
				ProjectID: "ibkr-trader",
				Symbol:    "NVDA",
				Qty:       4,
				Price:     221.38,
				FilledAt:  now.Add(-31 * time.Minute),
			},
		},
	}
	orders := &replayingOrderRepo{
		rows: []*persistence.TradingOrder{
			{
				ID:          "ord-1",
				ProjectID:   "ibkr-trader",
				Symbol:      "TSLA",
				Action:      "BUY",
				Status:      "filled",
				SubmittedAt: now.Add(-30 * time.Minute),
				LimitPrice:  &lp,
			},
			// ord-2 backs fill-2 — both fills reference a real
			// same-project order so the (new) orphan-fill drop does
			// not apply to this happy-path case.
			{
				ID:          "ord-2",
				ProjectID:   "ibkr-trader",
				Symbol:      "NVDA",
				Action:      "BUY",
				Status:      "filled",
				SubmittedAt: now.Add(-31 * time.Minute),
			},
		},
	}
	server := newServerWithRepos(fills, orders)

	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got stateReplayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v\nbody=%s", err, rec.Body.String())
	}
	if len(got.Fills) != 2 {
		t.Fatalf("expected 2 fills, got %d", len(got.Fills))
	}
	if got.Fills[0].Symbol != "TSLA" || got.Fills[0].Qty != 2 || got.Fills[0].Price != 434.10 {
		t.Fatalf("first fill not propagated correctly: %+v", got.Fills[0])
	}
	if len(got.Orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(got.Orders))
	}
	if got.Orders[0].ClientTag != "ord-1" || got.Orders[0].LimitPrice != 434.10 {
		t.Fatalf("order client_tag/limit not propagated: %+v", got.Orders[0])
	}
	if got.TodayUTC == "" || got.NowUTC == "" {
		t.Fatalf("today_utc and now_utc must be populated for the broker to pick the right dayTurnover key")
	}
}

// TestStateReplay_FiltersByCallerProject — cross-project access must
// 403 the same way ingest endpoints do. Without this, a leaked API key
// for project A could exfiltrate B's trading volume.
func TestStateReplay_FiltersByCallerProject(t *testing.T) {
	server := newServerWithRepos(&replayingFillRepo{}, &replayingOrderRepo{})
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=other-project", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-project access, got %d", rec.Code)
	}
}

// TestStateReplay_QueriesSinceMidnightUTC — the daily-turnover bucket
// rolls over at midnight UTC, so the fill query MUST use today-UTC-midnight
// as the floor. A bug that used local midnight or now-24h would either
// miss this morning's fills (after 02:00 CEST → today UTC) or double-count
// yesterday's late activity. Pin the contract.
func TestStateReplay_QueriesSinceMidnightUTC(t *testing.T) {
	fills := &replayingFillRepo{}
	orders := &replayingOrderRepo{}
	server := newServerWithRepos(fills, orders)
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)

	if !fills.receivedList {
		t.Fatal("expected fills repo to be queried")
	}
	if fills.lastFilter.Since == nil {
		t.Fatal("fill query must carry a Since filter")
	}
	since := fills.lastFilter.Since.UTC()
	now := time.Now().UTC()
	expected := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !since.Equal(expected) {
		t.Fatalf("Since must be today's UTC midnight (%s), got %s", expected, since)
	}
	if fills.lastFilter.ProjectID == nil || *fills.lastFilter.ProjectID != "ibkr-trader" {
		t.Fatalf("fill query must scope by project; got %+v", fills.lastFilter.ProjectID)
	}

	// Orders are queried for the last hour (rate-limit window).
	if orders.lastFilter.Since == nil {
		t.Fatal("order query must carry a Since filter")
	}
	orderSince := orders.lastFilter.Since.UTC()
	if now.Sub(orderSince) > 65*time.Minute || now.Sub(orderSince) < 55*time.Minute {
		t.Fatalf("order Since must be ~1h ago; got %s (delta %s)", orderSince, now.Sub(orderSince))
	}
}

// TestStateReplay_MissingProjectParam — bad input rejected cleanly.
func TestStateReplay_MissingProjectParam(t *testing.T) {
	server := newServerWithRepos(&replayingFillRepo{}, &replayingOrderRepo{})
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing project param, got %d", rec.Code)
	}
}

// TestStateReplay_MissingRepos — 503 when not wired (test setups
// without the repos still construct a Server). Same shape as the
// ingest endpoints' contract.
func TestStateReplay_MissingRepos(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()))
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when trading repos absent, got %d", rec.Code)
	}
}

// TestStateReplay_MethodNotAllowed — POST/DELETE etc must 405.
func TestStateReplay_MethodNotAllowed(t *testing.T) {
	server := newServerWithRepos(&replayingFillRepo{}, &replayingOrderRepo{})
	rec := httptest.NewRecorder()
	req := scopedRequest(http.MethodPost, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "{}", "ibkr-trader")
	server.GetTradingStateReplay(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for non-GET, got %d", rec.Code)
	}
}

// maxFilledAtFillRepo extends replayingFillRepo with a configurable
// MaxFilledAt return value for the new Task 11 tests.
type maxFilledAtFillRepo struct {
	replayingFillRepo
	maxFilledAt  time.Time
	maxFilledErr error
}

func (r *maxFilledAtFillRepo) MaxFilledAt(_ context.Context, _ string) (time.Time, error) {
	return r.maxFilledAt, r.maxFilledErr
}

// TestStateReplay_MaxFilledAt asserts that:
// (a) when MaxFilledAt returns a non-zero time the response carries
//
//	max_filled_at as a non-empty RFC3339 string;
//
// (b) when MaxFilledAt returns zero the field is absent / empty.
func TestStateReplay_MaxFilledAt(t *testing.T) {
	orders := &replayingOrderRepo{}

	t.Run("non-zero max_filled_at propagated", func(t *testing.T) {
		mfa := time.Date(2026, 6, 25, 14, 30, 0, 0, time.UTC)
		fills := &maxFilledAtFillRepo{maxFilledAt: mfa}
		server := NewServer(
			WithLogger(zerolog.Nop()),
			WithTradingFillRepository(fills),
			WithTradingOrderRepository(orders),
		)

		rec := httptest.NewRecorder()
		req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
		server.GetTradingStateReplay(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var got stateReplayResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.MaxFilledAt == "" {
			t.Fatalf("expected max_filled_at to be set, got empty string")
		}
		parsed, err := time.Parse(time.RFC3339, got.MaxFilledAt)
		if err != nil {
			t.Fatalf("max_filled_at is not valid RFC3339: %v", err)
		}
		if !parsed.Equal(mfa) {
			t.Fatalf("expected max_filled_at=%s, got %s", mfa.Format(time.RFC3339), got.MaxFilledAt)
		}
	})

	t.Run("zero MaxFilledAt yields empty field", func(t *testing.T) {
		fills := &maxFilledAtFillRepo{maxFilledAt: time.Time{}}
		server := NewServer(
			WithLogger(zerolog.Nop()),
			WithTradingFillRepository(fills),
			WithTradingOrderRepository(orders),
		)

		rec := httptest.NewRecorder()
		req := scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader")
		server.GetTradingStateReplay(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var got stateReplayResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if got.MaxFilledAt != "" {
			t.Fatalf("expected max_filled_at to be empty for zero time, got %q", got.MaxFilledAt)
		}
	})
}
