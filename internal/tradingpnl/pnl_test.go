package tradingpnl_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/tradingpnl"
)

// day returns a fixed UTC base date + n days.
func day(n int) time.Time {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(n) * 24 * time.Hour)
}

// ptr returns a pointer to a float64.
func ptr(f float64) *float64 { return &f }

// ─────────────────────────────────────────────────────────────────────────────
// PairRoundTrips — Task 1.1: long round-trip
// ─────────────────────────────────────────────────────────────────────────────

func TestPairRoundTrips_LongWin(t *testing.T) {
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "long", got[0].Side)
	assert.InDelta(t, 100.0, got[0].RealizedUSD, 1e-9) // (110-100)*10
	assert.Equal(t, day(2), got[0].ExitAt)
	assert.Equal(t, day(1), got[0].EntryAt)
	assert.Equal(t, "SPY", got[0].Symbol)
	assert.InDelta(t, 100.0, got[0].EntryPx, 1e-9)
	assert.InDelta(t, 110.0, got[0].ExitPx, 1e-9)
	assert.InDelta(t, 10.0, got[0].Qty, 1e-9)
}

func TestPairRoundTrips_LongLoss(t *testing.T) {
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "AAPL", Qty: 5, Price: 200, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "AAPL", Qty: 5, Price: 180, FilledAt: day(3)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "long", got[0].Side)
	assert.InDelta(t, -100.0, got[0].RealizedUSD, 1e-9) // (180-200)*5
}

// ─────────────────────────────────────────────────────────────────────────────
// PairRoundTrips — Task 1.2: short, partial, commissions, open-lot, multi-symbol
// ─────────────────────────────────────────────────────────────────────────────

func TestPairRoundTrips_ShortWin(t *testing.T) {
	// SELL to open, BUY to close; profit = (entry-exit)*qty
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "SELL", "o2": "BUY"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "short", got[0].Side)
	assert.InDelta(t, 100.0, got[0].RealizedUSD, 1e-9) // (110-100)*10
}

func TestPairRoundTrips_ShortLoss(t *testing.T) {
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "SELL", "o2": "BUY"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "short", got[0].Side)
	assert.InDelta(t, -100.0, got[0].RealizedUSD, 1e-9) // (100-110)*10
}

func TestPairRoundTrips_PartialClose(t *testing.T) {
	// Open 10, close 4 → one trade qty=4, lot keeps 6
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 4, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.InDelta(t, 4.0, got[0].Qty, 1e-9)
	assert.InDelta(t, 40.0, got[0].RealizedUSD, 1e-9) // (110-100)*4
}

func TestPairRoundTrips_CommissionsNetted(t *testing.T) {
	// Entry fill: 10 shares, commission $2. Exit fill: 10 shares, commission $1.
	// Gross profit = (110-100)*10 = 100; net = 100 - 2 - 1 = 97
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, CommissionUSD: ptr(2.0), FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, CommissionUSD: ptr(1.0), FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.InDelta(t, 97.0, got[0].RealizedUSD, 1e-9)
}

func TestPairRoundTrips_PartialCommissionProRated(t *testing.T) {
	// Entry fill: 10 shares, commission $2 (= $0.20/share).
	// Partial close: 4 shares, commission $1 (= $0.25/share for the close leg).
	// Gross profit = (110-100)*4 = 40
	// Entry commission portion = 2 * (4/10) = 0.8
	// Exit commission = 1 * (4/4) = 1.0
	// Net = 40 - 0.8 - 1.0 = 38.2
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, CommissionUSD: ptr(2.0), FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 4, Price: 110, CommissionUSD: ptr(1.0), FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.InDelta(t, 38.2, got[0].RealizedUSD, 1e-9)
}

func TestPairRoundTrips_OpenLotExcluded(t *testing.T) {
	// BUY with no closing SELL → not emitted
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
	}
	side := map[string]string{"o1": "BUY"}
	got := tradingpnl.PairRoundTrips(fills, side)
	assert.Empty(t, got)
}

func TestPairRoundTrips_MultiSymbolIsolation(t *testing.T) {
	// SPY and AAPL fills don't cross-match
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "AAPL", Qty: 5, Price: 200, FilledAt: day(1)},
		{OrderID: "o3", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
		// AAPL has no close → excluded
	}
	side := map[string]string{"o1": "BUY", "o2": "BUY", "o3": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "SPY", got[0].Symbol)
	assert.InDelta(t, 100.0, got[0].RealizedUSD, 1e-9)
}

func TestPairRoundTrips_FIFOOrder(t *testing.T) {
	// Two opens: buy 10@100 then buy 10@105; one close: sell 10@110.
	// FIFO: the close matches the FIRST open (entry=100, exit=110, realized=100).
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 105, FilledAt: day(2)},
		{OrderID: "o3", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(3)},
	}
	side := map[string]string{"o1": "BUY", "o2": "BUY", "o3": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.InDelta(t, 100.0, got[0].EntryPx, 1e-9) // matched oldest lot
	assert.InDelta(t, 100.0, got[0].RealizedUSD, 1e-9)
}

func TestPairRoundTrips_MissingOrderIDSkipped(t *testing.T) {
	// fill o2's OrderID is absent from sideByOrderID → skip it
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY"} // o2 missing
	got := tradingpnl.PairRoundTrips(fills, side)
	assert.Empty(t, got) // the BUY stays open, no close → nothing emitted
}

func TestPairRoundTrips_CaseInsensitiveAction(t *testing.T) {
	// broker may send lowercase
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "buy", "o2": "sell"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.Equal(t, "long", got[0].Side)
	assert.InDelta(t, 100.0, got[0].RealizedUSD, 1e-9)
}

func TestPairRoundTrips_PartialThenFullClose(t *testing.T) {
	// Open 10; partial close 4; then full close of remaining 6.
	// Should emit two trades.
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 4, Price: 110, FilledAt: day(2)},
		{OrderID: "o3", Symbol: "SPY", Qty: 6, Price: 115, FilledAt: day(3)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL", "o3": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 2)
	assert.InDelta(t, 4.0, got[0].Qty, 1e-9)
	assert.InDelta(t, 40.0, got[0].RealizedUSD, 1e-9) // (110-100)*4
	assert.InDelta(t, 6.0, got[1].Qty, 1e-9)
	assert.InDelta(t, 90.0, got[1].RealizedUSD, 1e-9) // (115-100)*6
}

func TestPairRoundTrips_ExactlyZeroRealizedNoExact(t *testing.T) {
	// Buy and sell at the same price → realized = 0
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 5, Price: 100, FilledAt: day(1)},
		{OrderID: "o2", Symbol: "SPY", Qty: 5, Price: 100, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	require.Len(t, got, 1)
	assert.InDelta(t, 0.0, got[0].RealizedUSD, 1e-9)
}

// ─────────────────────────────────────────────────────────────────────────────
// Aggregate — Task 1.3
// ─────────────────────────────────────────────────────────────────────────────

func TestAggregate_BasicMetrics(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(1)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 50, ExitAt: day(2)},
		{Symbol: "AAPL", Side: "long", Qty: 1, RealizedUSD: -30, ExitAt: day(3)},
	}
	since := day(0)
	until := day(10)

	got := tradingpnl.Aggregate(trades, since, until)

	assert.Equal(t, 3, got.Trades)
	assert.Equal(t, 2, got.Wins)
	assert.Equal(t, 1, got.Losses)
	assert.InDelta(t, 66.666, got.WinRatePct, 0.001)
	assert.InDelta(t, 150.0, got.GrossWonUSD, 1e-9)
	assert.InDelta(t, 30.0, got.GrossLostUSD, 1e-9)
	assert.InDelta(t, 120.0, got.NetRealizedUSD, 1e-9)
	assert.InDelta(t, 75.0, got.AvgWinUSD, 1e-9)
	assert.InDelta(t, 30.0, got.AvgLossUSD, 1e-9)
	require.NotNil(t, got.ProfitFactor)
	assert.InDelta(t, 5.0, *got.ProfitFactor, 1e-9)
}

func TestAggregate_ZeroLossProfitFactorNil(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(1)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	assert.Equal(t, 1, got.Wins)
	assert.Equal(t, 0, got.Losses)
	assert.Nil(t, got.ProfitFactor) // no losses → nil, not ∞
}

func TestAggregate_ExactlyZeroRealizedCountsAsTradeNotWinLoss(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 0, ExitAt: day(1)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	assert.Equal(t, 1, got.Trades)
	assert.Equal(t, 0, got.Wins)
	assert.Equal(t, 0, got.Losses)
	assert.Nil(t, got.ProfitFactor)
}

func TestAggregate_OutOfWindowExcluded(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(5)},  // in window [3,8)
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 200, ExitAt: day(2)},  // before window
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 300, ExitAt: day(8)},  // at until → excluded (half-open)
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 400, ExitAt: day(10)}, // after window
	}
	got := tradingpnl.Aggregate(trades, day(3), day(8))
	assert.Equal(t, 1, got.Trades)
	assert.InDelta(t, 100.0, got.NetRealizedUSD, 1e-9)
}

func TestAggregate_DailySeries(t *testing.T) {
	// Two trades on day(1), one on day(2)
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(1)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 50, ExitAt: day(1)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: -30, ExitAt: day(2)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	require.Len(t, got.Daily, 2)
	// Day entries should be ordered chronologically
	assert.Equal(t, day(1).Truncate(24*time.Hour), got.Daily[0].Day)
	assert.InDelta(t, 150.0, got.Daily[0].RealizedUSD, 1e-9)
	assert.Equal(t, day(2).Truncate(24*time.Hour), got.Daily[1].Day)
	assert.InDelta(t, -30.0, got.Daily[1].RealizedUSD, 1e-9)
}

func TestAggregate_BySymbolSortedNetDesc(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 50, ExitAt: day(1)},
		{Symbol: "AAPL", Side: "long", Qty: 1, RealizedUSD: 200, ExitAt: day(2)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: -10, ExitAt: day(3)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	require.Len(t, got.BySymbol, 2)
	// AAPL net=200 > SPY net=40 → AAPL first
	assert.Equal(t, "AAPL", got.BySymbol[0].Symbol)
	assert.InDelta(t, 200.0, got.BySymbol[0].NetRealizedUSD, 1e-9)
	assert.Equal(t, "SPY", got.BySymbol[1].Symbol)
	assert.InDelta(t, 40.0, got.BySymbol[1].NetRealizedUSD, 1e-9)
}

func TestAggregate_BySymbolMetrics(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(1)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: -40, ExitAt: day(2)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	require.Len(t, got.BySymbol, 1)
	s := got.BySymbol[0]
	assert.Equal(t, "SPY", s.Symbol)
	assert.Equal(t, 2, s.Trades)
	assert.Equal(t, 1, s.Wins)
	assert.Equal(t, 1, s.Losses)
	assert.InDelta(t, 50.0, s.WinRatePct, 0.001)
	assert.InDelta(t, 100.0, s.GrossWonUSD, 1e-9)
	assert.InDelta(t, 40.0, s.GrossLostUSD, 1e-9)
	assert.InDelta(t, 60.0, s.NetRealizedUSD, 1e-9)
}

func TestAggregate_EmptyTrades(t *testing.T) {
	got := tradingpnl.Aggregate(nil, day(0), day(10))
	assert.Equal(t, 0, got.Trades)
	assert.Nil(t, got.ProfitFactor)
	assert.Empty(t, got.Daily)
	assert.Empty(t, got.BySymbol)
}

func TestAggregate_AvgWinAvgLoss(t *testing.T) {
	trades := []tradingpnl.ClosedTrade{
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 100, ExitAt: day(1)},
		{Symbol: "SPY", Side: "long", Qty: 1, RealizedUSD: 200, ExitAt: day(2)},
		{Symbol: "AAPL", Side: "long", Qty: 1, RealizedUSD: -60, ExitAt: day(3)},
		{Symbol: "AAPL", Side: "long", Qty: 1, RealizedUSD: -40, ExitAt: day(4)},
	}
	got := tradingpnl.Aggregate(trades, day(0), day(10))
	assert.InDelta(t, 150.0, got.AvgWinUSD, 1e-9) // (100+200)/2
	assert.InDelta(t, 50.0, got.AvgLossUSD, 1e-9) // (60+40)/2
}

// ─────────────────────────────────────────────────────────────────────────────
// EquityCurve — Task 1.4
// ─────────────────────────────────────────────────────────────────────────────

func TestEquityCurve_LastOfDayUsed(t *testing.T) {
	// Multiple snapshots on day(1): should use the last one (13:00)
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(1).Add(-2 * time.Hour), EquityUSD: 10000}, // 10:00 on day(1)
		{RecordedAt: day(1).Add(1 * time.Hour), EquityUSD: 10050},  // 13:00 on day(1)
		{RecordedAt: day(2), EquityUSD: 10200},
	}
	got := tradingpnl.EquityCurve(snaps, day(0), day(3))
	require.Len(t, got, 2)
	// day(1) point uses the last snapshot's equity (10050)
	assert.InDelta(t, 10050.0, got[0].EquityUSD, 1e-9)
	// RealizedUSD is always 0 (EquityCurve doesn't own realized)
	assert.InDelta(t, 0.0, got[0].RealizedUSD, 1e-9)
}

func TestEquityCurve_OutOfWindowExcluded(t *testing.T) {
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(1), EquityUSD: 9000},  // before window [2,4)
		{RecordedAt: day(2), EquityUSD: 10000}, // in window
		{RecordedAt: day(3), EquityUSD: 10500}, // in window
		{RecordedAt: day(4), EquityUSD: 11000}, // at until → excluded
	}
	got := tradingpnl.EquityCurve(snaps, day(2), day(4))
	require.Len(t, got, 2)
	assert.InDelta(t, 10000.0, got[0].EquityUSD, 1e-9)
	assert.InDelta(t, 10500.0, got[1].EquityUSD, 1e-9)
}

func TestEquityCurve_OrderedByDay(t *testing.T) {
	// Snapshots out of order → result ordered by day ascending
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(3), EquityUSD: 10500},
		{RecordedAt: day(1), EquityUSD: 10000},
		{RecordedAt: day(2), EquityUSD: 10200},
	}
	got := tradingpnl.EquityCurve(snaps, day(0), day(5))
	require.Len(t, got, 3)
	assert.True(t, got[0].Day.Before(got[1].Day))
	assert.True(t, got[1].Day.Before(got[2].Day))
}

func TestEquityCurve_SingleSnapshot(t *testing.T) {
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(1), EquityUSD: 12345.67},
	}
	got := tradingpnl.EquityCurve(snaps, day(0), day(5))
	require.Len(t, got, 1)
	assert.InDelta(t, 12345.67, got[0].EquityUSD, 1e-6)
	assert.InDelta(t, 0.0, got[0].RealizedUSD, 1e-9)
}

func TestEquityCurve_EmptySnaps(t *testing.T) {
	got := tradingpnl.EquityCurve(nil, day(0), day(10))
	assert.Empty(t, got)
}

func TestEquityCurve_DayTruncatedToUTCMidnight(t *testing.T) {
	// Snapshot at 14:30 on day(1) should produce Day = UTC midnight of day(1)
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(1).Add(2*time.Hour + 30*time.Minute), EquityUSD: 9999},
	}
	got := tradingpnl.EquityCurve(snaps, day(0), day(5))
	require.Len(t, got, 1)
	// Day should be UTC midnight (truncated)
	expected := day(1).Truncate(24 * time.Hour)
	assert.Equal(t, expected, got[0].Day)
}

func TestEquityCurve_MultipleOnSameDay_LastWins(t *testing.T) {
	snaps := []*persistence.TradingPositionsSnapshot{
		{RecordedAt: day(1).Add(0), EquityUSD: 1000},             // first on day
		{RecordedAt: day(1).Add(1 * time.Hour), EquityUSD: 2000}, // second on day
		{RecordedAt: day(1).Add(2 * time.Hour), EquityUSD: 3000}, // last on day
	}
	got := tradingpnl.EquityCurve(snaps, day(0), day(5))
	require.Len(t, got, 1)
	assert.InDelta(t, 3000.0, got[0].EquityUSD, 1e-9)
}

// A nil fill in the slice must be skipped, not panic — fill lists can carry
// nil rows (the UI trading panel injects/handles them).
func TestPairRoundTrips_SkipsNilFills(t *testing.T) {
	fills := []*persistence.TradingFill{
		{OrderID: "o1", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: day(1)},
		nil,
		{OrderID: "o2", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: day(2)},
	}
	side := map[string]string{"o1": "BUY", "o2": "SELL"}
	got := tradingpnl.PairRoundTrips(fills, side)
	if len(got) != 1 {
		t.Fatalf("expected 1 closed trade despite the nil fill, got %d", len(got))
	}
}
