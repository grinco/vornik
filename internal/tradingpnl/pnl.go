// Package tradingpnl provides pure (no-I/O) FIFO round-trip P&L computation,
// aggregation, and equity-curve reduction for vornik's trading reports.
// All functions are deterministic and exhaustively table-tested.
//
// It is a neutral CE leaf (Phase 2c): its Performance/DailyPoint types are
// embedded in CE internal/ui structs, so it must stay CE-visible even though
// the daemon-side trading subsystem relocated to internal/enterprise/trading.
package tradingpnl

import (
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// ClosedTrade is one matched round-trip (open + close) for a single symbol.
// RealizedUSD is net of the pro-rated commissions on both the entry and exit fills.
type ClosedTrade struct {
	Symbol      string
	Side        string // "long" | "short" (the opening side)
	Qty         float64
	EntryPx     float64
	ExitPx      float64
	EntryAt     time.Time
	ExitAt      time.Time
	RealizedUSD float64 // net of pro-rated commissions on both legs
}

// DailyPoint is one calendar day's realized P&L and mark-to-market equity.
// RealizedUSD is populated by Aggregate; EquityUSD by EquityCurve.
type DailyPoint struct {
	Day         time.Time
	RealizedUSD float64
	EquityUSD   float64
}

// SymbolPerformance is the same metric set as Performance, scoped to a
// single ticker symbol.
type SymbolPerformance struct {
	Symbol         string
	Trades         int
	Wins           int
	Losses         int
	WinRatePct     float64
	NetRealizedUSD float64
	GrossWonUSD    float64
	GrossLostUSD   float64
}

// Performance is the full aggregated metric set for a time window.
type Performance struct {
	Trades         int
	Wins           int
	Losses         int
	WinRatePct     float64
	GrossWonUSD    float64
	GrossLostUSD   float64
	NetRealizedUSD float64
	AvgWinUSD      float64
	AvgLossUSD     float64
	ProfitFactor   *float64 // nil when GrossLost == 0 (never ∞/NaN)
	Daily          []DailyPoint
	BySymbol       []SymbolPerformance
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// openLot is one unmatched entry lot sitting in the FIFO queue.
type openLot struct {
	side          string  // "long" | "short"
	qty           float64 // remaining unmatched quantity
	totalQty      float64 // original fill qty (for commission pro-rating)
	price         float64
	at            time.Time
	commissionUSD float64 // total commission for the fill (0 if nil)
}

// symbolQueues holds FIFO open-lot queues per symbol.
type symbolQueues struct {
	long  []openLot
	short []openLot
}

// normAction normalises BUY/SELL action strings case-insensitively to uppercase.
func normAction(a string) string {
	return strings.ToUpper(strings.TrimSpace(a))
}

// commVal returns the commission value, treating nil as 0.
func commVal(c *float64) float64 {
	if c == nil {
		return 0
	}
	return *c
}

// utcDay truncates a time to UTC midnight (the attribution day).
func utcDay(t time.Time) time.Time {
	return t.UTC().Truncate(24 * time.Hour)
}

// matchClose pops lots from the given queue, emitting ClosedTrade values for
// each matched quantity. openSide is "long" or "short"; realized is computed
// as (exitPx−entryPx)*qty for long, (entryPx−exitPx)*qty for short.
// Returns the filled trades and the remaining unmatched quantity from the
// closing fill.
func matchClose(
	lots *[]openLot,
	openSide string,
	fillQty, fillPrice float64,
	fillAt time.Time,
	fillComm float64,
	sym string,
) ([]ClosedTrade, float64) {
	remaining := fillQty
	var trades []ClosedTrade

	for remaining > 0 && len(*lots) > 0 {
		lot := &(*lots)[0]
		matched := min(remaining, lot.qty)

		entryComm := lot.commissionUSD * (matched / lot.totalQty)
		exitComm := fillComm * (matched / fillQty)

		var realized float64
		if openSide == "long" {
			realized = (fillPrice-lot.price)*matched - entryComm - exitComm
		} else {
			realized = (lot.price-fillPrice)*matched - entryComm - exitComm
		}

		trades = append(trades, ClosedTrade{
			Symbol:      sym,
			Side:        openSide,
			Qty:         matched,
			EntryPx:     lot.price,
			ExitPx:      fillPrice,
			EntryAt:     lot.at,
			ExitAt:      fillAt,
			RealizedUSD: realized,
		})

		lot.qty -= matched
		if lot.qty <= 0 {
			*lots = (*lots)[1:]
		}
		remaining -= matched
	}

	return trades, remaining
}

// ─────────────────────────────────────────────────────────────────────────────
// PairRoundTrips
// ─────────────────────────────────────────────────────────────────────────────

// PairRoundTrips groups fills by symbol and FIFO-matches opens against closes,
// emitting one ClosedTrade per matched lot (or lot fraction for partial closes).
//
// sideByOrderID maps fill.OrderID → the order's Action ("BUY" or "SELL";
// matching is case-insensitive). A fill whose OrderID is absent from the map
// is silently skipped. Symbols never cross-match.
//
// A BUY opens a long lot (or closes the oldest short lot).
// A SELL opens a short lot (or closes the oldest long lot).
//
// RealizedUSD for a long = (exitPx − entryPx) × qty − pro-rated commissions.
// RealizedUSD for a short = (entryPx − exitPx) × qty − pro-rated commissions.
//
// Open lots with no matching close are not emitted (unrealized; captured by
// EquityCurve).
func PairRoundTrips(fills []*persistence.TradingFill, sideByOrderID map[string]string) []ClosedTrade {
	queues := make(map[string]*symbolQueues)
	var trades []ClosedTrade

	for _, f := range fills {
		if f == nil {
			continue // defensive: fill lists may carry nil rows (see UI panel)
		}
		rawAction, ok := sideByOrderID[f.OrderID]
		if !ok {
			continue
		}
		action := normAction(rawAction)

		sym := f.Symbol
		if queues[sym] == nil {
			queues[sym] = &symbolQueues{}
		}
		sq := queues[sym]
		comm := commVal(f.CommissionUSD)

		switch action {
		case "BUY":
			closed, remaining := matchClose(&sq.short, "short", f.Qty, f.Price, f.FilledAt, comm, sym)
			trades = append(trades, closed...)
			if remaining > 0 {
				openComm := comm * (remaining / f.Qty)
				sq.long = append(sq.long, openLot{
					side: "long", qty: remaining, totalQty: remaining,
					price: f.Price, at: f.FilledAt, commissionUSD: openComm,
				})
			}
		case "SELL":
			closed, remaining := matchClose(&sq.long, "long", f.Qty, f.Price, f.FilledAt, comm, sym)
			trades = append(trades, closed...)
			if remaining > 0 {
				openComm := comm * (remaining / f.Qty)
				sq.short = append(sq.short, openLot{
					side: "short", qty: remaining, totalQty: remaining,
					price: f.Price, at: f.FilledAt, commissionUSD: openComm,
				})
			}
		}
	}

	return trades
}

// ─────────────────────────────────────────────────────────────────────────────
// Aggregate
// ─────────────────────────────────────────────────────────────────────────────

// symAcc accumulates per-symbol metrics during Aggregate.
type symAcc struct {
	trades, wins, losses int
	grossWon, grossLost  float64
	net                  float64
}

func (s *symAcc) add(realized float64) {
	s.trades++
	s.net += realized
	switch {
	case realized > 0:
		s.wins++
		s.grossWon += realized
	case realized < 0:
		s.losses++
		s.grossLost += -realized
	}
}

func (s *symAcc) toSymbolPerformance(sym string) SymbolPerformance {
	sp := SymbolPerformance{
		Symbol:         sym,
		Trades:         s.trades,
		Wins:           s.wins,
		Losses:         s.losses,
		NetRealizedUSD: s.net,
		GrossWonUSD:    s.grossWon,
		GrossLostUSD:   s.grossLost,
	}
	if s.trades > 0 {
		sp.WinRatePct = float64(s.wins) / float64(s.trades) * 100
	}
	return sp
}

// buildDailySeries converts a day→realized map to a sorted slice.
func buildDailySeries(dailyMap map[time.Time]float64) []DailyPoint {
	days := make([]time.Time, 0, len(dailyMap))
	for d := range dailyMap {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	pts := make([]DailyPoint, len(days))
	for i, d := range days {
		pts[i] = DailyPoint{Day: d, RealizedUSD: dailyMap[d]}
	}
	return pts
}

// buildBySymbol converts a symbol→acc map to a sorted slice (net desc).
func buildBySymbol(symMap map[string]*symAcc) []SymbolPerformance {
	syms := make([]SymbolPerformance, 0, len(symMap))
	for sym, s := range symMap {
		syms = append(syms, s.toSymbolPerformance(sym))
	}
	sort.Slice(syms, func(i, j int) bool {
		if syms[i].NetRealizedUSD != syms[j].NetRealizedUSD {
			return syms[i].NetRealizedUSD > syms[j].NetRealizedUSD
		}
		return syms[i].Symbol < syms[j].Symbol
	})
	return syms
}

// Aggregate filters closed trades to ExitAt ∈ [since, until) and computes the
// full metric set, a daily realized series, and a per-symbol rollup.
//
// ProfitFactor is nil when GrossLostUSD == 0 (never returns ∞ or NaN).
// Daily is sorted chronologically; BySymbol is sorted by NetRealizedUSD desc.
func Aggregate(trades []ClosedTrade, since, until time.Time) Performance {
	var p Performance
	dailyMap := make(map[time.Time]float64)
	symMap := make(map[string]*symAcc)

	for i := range trades {
		t := &trades[i]
		if t.ExitAt.Before(since) || !t.ExitAt.Before(until) {
			continue
		}
		p.Trades++
		p.NetRealizedUSD += t.RealizedUSD
		switch {
		case t.RealizedUSD > 0:
			p.Wins++
			p.GrossWonUSD += t.RealizedUSD
		case t.RealizedUSD < 0:
			p.Losses++
			p.GrossLostUSD += -t.RealizedUSD
		}
		dailyMap[utcDay(t.ExitAt)] += t.RealizedUSD
		if symMap[t.Symbol] == nil {
			symMap[t.Symbol] = &symAcc{}
		}
		symMap[t.Symbol].add(t.RealizedUSD)
	}

	if p.Trades == 0 {
		return p
	}

	p.WinRatePct = float64(p.Wins) / float64(p.Trades) * 100
	if p.Wins > 0 {
		p.AvgWinUSD = p.GrossWonUSD / float64(p.Wins)
	}
	if p.Losses > 0 {
		p.AvgLossUSD = p.GrossLostUSD / float64(p.Losses)
	}
	if p.GrossLostUSD > 0 {
		pf := p.GrossWonUSD / p.GrossLostUSD
		p.ProfitFactor = &pf
	}
	p.Daily = buildDailySeries(dailyMap)
	p.BySymbol = buildBySymbol(symMap)
	return p
}

// ─────────────────────────────────────────────────────────────────────────────
// EquityCurve
// ─────────────────────────────────────────────────────────────────────────────

// EquityCurve reduces snapshots to one DailyPoint per UTC day (using each day's
// last snapshot's EquityUSD), filtered to [since, until), ordered by day.
//
// RealizedUSD is always 0 on the returned points — Aggregate owns realized.
func EquityCurve(snaps []*persistence.TradingPositionsSnapshot, since, until time.Time) []DailyPoint {
	type dayEntry struct {
		snap *persistence.TradingPositionsSnapshot
	}
	dayMap := make(map[time.Time]dayEntry)

	for _, s := range snaps {
		if s.RecordedAt.Before(since) || !s.RecordedAt.Before(until) {
			continue
		}
		d := utcDay(s.RecordedAt)
		existing, ok := dayMap[d]
		if !ok || s.RecordedAt.After(existing.snap.RecordedAt) {
			dayMap[d] = dayEntry{snap: s}
		}
	}

	if len(dayMap) == 0 {
		return nil
	}

	days := make([]time.Time, 0, len(dayMap))
	for d := range dayMap {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	result := make([]DailyPoint, len(days))
	for i, d := range days {
		result[i] = DailyPoint{
			Day:       d,
			EquityUSD: dayMap[d].snap.EquityUSD,
			// RealizedUSD intentionally 0 — Aggregate owns realized.
		}
	}
	return result
}
