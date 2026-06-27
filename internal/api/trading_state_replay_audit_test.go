package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// TestStateReplay_DropsOrphanAndCrossProjectFills guards the fix that the
// state-replay only feeds the live dayTurnover replay with fills that
// reference an order THIS project recorded today. The trading_fills.order_id
// FK proves existence, not project ownership, so a forged/cross-project fill
// could otherwise be replayed straight into the safety envelope's turnover
// cap. We fail closed: orphan + cross-project fills are dropped.
func TestStateReplay_DropsOrphanAndCrossProjectFills(t *testing.T) {
	now := time.Now().UTC()
	fills := &replayingFillRepo{
		rows: []*persistence.TradingFill{
			// Legit: references ord-real, owned by this project.
			{ID: "f-ok", OrderID: "ord-real", ProjectID: "ibkr-trader", Symbol: "TSLA", Qty: 1, Price: 100, FilledAt: now.Add(-10 * time.Minute)},
			// Forged orphan: references an order id this project never recorded.
			{ID: "f-orphan", OrderID: "ord-forged", ProjectID: "ibkr-trader", Symbol: "NVDA", Qty: 9, Price: 9999, FilledAt: now.Add(-9 * time.Minute)},
			// Cross-project: a row whose ProjectID is not the caller's.
			{ID: "f-cross", OrderID: "ord-real", ProjectID: "other-project", Symbol: "TSLA", Qty: 5, Price: 100, FilledAt: now.Add(-8 * time.Minute)},
		},
	}
	orders := &replayingOrderRepo{
		rows: []*persistence.TradingOrder{
			{ID: "ord-real", ProjectID: "ibkr-trader", Symbol: "TSLA", Action: "BUY", Status: "filled", SubmittedAt: now.Add(-10 * time.Minute)},
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
		t.Fatalf("bad JSON: %v", err)
	}
	// Only the legit fill survives; the forged orphan and the cross-project
	// row must NOT feed the turnover replay.
	if len(got.Fills) != 1 {
		t.Fatalf("expected exactly 1 surviving fill, got %d: %+v", len(got.Fills), got.Fills)
	}
	if got.Fills[0].Symbol != "TSLA" || got.Fills[0].Qty != 1 {
		t.Errorf("wrong fill survived (a forged row leaked into turnover): %+v", got.Fills[0])
	}
}
