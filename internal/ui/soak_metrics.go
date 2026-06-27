package ui

import (
	"math"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// SoakMetrics is the rollup the trading panel surfaces from
// the equity-snapshot series. Computed in computeSoakMetrics —
// kept on its own type so the math is testable without the
// HTTP / template machinery.
//
// The series itself comes from
// trading_positions_snapshots (one row per project per sampler
// tick, default 5 min). Sharpe is calculated daily-closing per
// the standard convention; max drawdown walks the full intra-
// window equity series so even a transient mid-day dip
// surfaces.
type SoakMetrics struct {
	// SampleCount is the number of snapshot rows in the
	// computed window. Operators see this directly so a low
	// number ("3 snapshots") makes "Sharpe = 0" non-confusing.
	SampleCount int
	// WindowDays counts unique UTC dates represented in the
	// series. Sharpe needs ≥5; below that, SoakReady is false
	// and the panel renders an "insufficient data" hint
	// instead of a deceptive zero.
	WindowDays int
	// SoakReady is true when WindowDays >= 5 — that's the
	// minimum for a 4-return Sharpe whose stddev isn't sample
	// noise. Below that the panel hides the headline number
	// and shows a "soak warming up" hint instead.
	SoakReady bool
	// SharpeAnnualized is the Sharpe ratio computed from
	// daily-closing equity log-returns, annualised by
	// sqrt(252). Risk-free rate assumed zero — operators
	// running a multi-symbol equity strategy don't get to
	// claim the T-bill spread back from their alpha.
	SharpeAnnualized float64
	// MaxDrawdownPct is the deepest peak-to-trough decline
	// (as a positive percentage) over the snapshot window.
	// Expressed against the running high-water mark, NOT the
	// session-open figure the broker exposes — so a 30-day
	// drawdown that doesn't reset overnight shows up here
	// even though the broker's drawdown_pct went back to 0.
	MaxDrawdownPct float64
	// Equity24hChangePct is the equity delta over the last
	// 24h, as a percentage. Computed as (last - first_in_24h)
	// / first_in_24h * 100. Zero when fewer than two
	// snapshots fall in the 24h window.
	Equity24hChangePct float64
	// Equity24hChangeUSD is the same delta as Equity24hChangePct
	// but in absolute dollars. Surfaced alongside the percent so
	// $30-on-$1M moves don't read as a flat "+0.00%" — sub-cent
	// percent precision rounds away information the operator
	// cares about for paper / small-equity accounts.
	Equity24hChangeUSD float64
	// OrdersToday / Orders7d are counts from trading_orders
	// over those windows. Includes refused orders so the
	// total reflects every place_order call the broker saw,
	// not just landed-at-IBKR ones. Zero when the broker
	// hasn't been writing orders yet (Phase 1 of the
	// broker→daemon audit channel).
	OrdersToday int64
	Orders7d    int64
	// VolumeToday / Volume7dUSD are sums of qty × estimated-
	// price across orders in the window. Filled / partial
	// orders use avg_fill_price when available; submitted
	// orders use limit_price (LMT) or 0 (MKT — we don't
	// know the actual fill yet). The number is approximate
	// — for a precise post-trade volume the operator joins
	// trading_fills (Phase 3); this is a fast-path estimate.
	VolumeTodayUSD float64
	Volume7dUSD    float64
	// FirstSampleAt / LastSampleAt are the time bounds of the
	// window the metrics were computed over. Surfaced in the
	// panel footer so an operator can see "soak window so
	// far: 2026-04-30 → today, 4 days" without doing the
	// math.
	FirstSampleAt time.Time
	LastSampleAt  time.Time
}

// dailyClose is one calendar-day-closing equity sample —
// what Sharpe is computed from. Defined at package scope (not
// inline) so the helper functions can take it as a typed slice
// without a struct-literal mismatch.
type dailyClose struct {
	date   string
	equity float64
	at     time.Time
}

// computeSoakMetrics turns a snapshot series (oldest first)
// into the rollup. Empty / nil input returns a zero-valued
// struct (SoakReady=false). Sharpe needs ≥5 distinct UTC
// dates; below that the headline value stays zero. Math
// follows the cross-listed convention: log returns, sqrt(252)
// annualisation, sample stddev (n-1) so a flat-equity sample
// pair doesn't divide by zero.
func computeSoakMetrics(snaps []*persistence.TradingPositionsSnapshot) SoakMetrics {
	out := SoakMetrics{}
	if len(snaps) == 0 {
		return out
	}
	out.SampleCount = len(snaps)
	out.FirstSampleAt = snaps[0].RecordedAt
	out.LastSampleAt = snaps[len(snaps)-1].RecordedAt

	// Pick the LAST snapshot of each UTC date. Walks once
	// because the input is sorted oldest-first; track the
	// running date, when it changes commit the previous-date
	// closing equity.
	var daily []dailyClose
	currentDate := ""
	var pending dailyClose
	for _, s := range snaps {
		if s == nil || s.EquityUSD <= 0 {
			continue
		}
		d := s.RecordedAt.UTC().Format("2006-01-02")
		if d != currentDate {
			if currentDate != "" {
				daily = append(daily, pending)
			}
			currentDate = d
		}
		pending = dailyClose{date: d, equity: s.EquityUSD, at: s.RecordedAt}
	}
	if currentDate != "" {
		daily = append(daily, pending)
	}
	out.WindowDays = len(daily)

	// Sharpe needs ≥5 distinct daily closes (4 returns) to
	// compute a stddev that isn't dominated by sample noise.
	// Below that we surface SampleCount + WindowDays so the
	// operator sees the soak is still warming up.
	if out.WindowDays >= 5 {
		out.SoakReady = true
		out.SharpeAnnualized = annualisedSharpe(daily)
	}

	// Max drawdown: walk the full snapshot series (NOT the
	// daily closes) so an intra-day dip is captured even when
	// the close returns to flat. Threshold for compute is
	// just len > 0 — a single sample produces 0% drawdown
	// trivially.
	out.MaxDrawdownPct = maxDrawdownPct(snaps)

	// Equity 24h change: find the oldest snapshot that's
	// within 24h of LastSampleAt; if there's one, take pct
	// change to the latest equity.
	out.Equity24hChangePct, out.Equity24hChangeUSD = equity24hChange(snaps)

	return out
}

func annualisedSharpe(daily []dailyClose) float64 {
	if len(daily) < 2 {
		return 0
	}
	// Log returns: ln(eq_i / eq_{i-1}). Skips entries where
	// the prior close is non-positive (defensive — equity
	// should never be ≤ 0 but a corrupt sample shouldn't
	// crash the math).
	var returns []float64
	for i := 1; i < len(daily); i++ {
		if daily[i-1].equity <= 0 {
			continue
		}
		r := math.Log(daily[i].equity / daily[i-1].equity)
		returns = append(returns, r)
	}
	if len(returns) < 2 {
		return 0
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))
	variance := 0.0
	for _, r := range returns {
		d := r - mean
		variance += d * d
	}
	// Sample stddev (n-1) — unbiased estimator for the
	// population stddev, matches the convention every
	// portfolio-analytics library uses.
	variance /= float64(len(returns) - 1)
	stddev := math.Sqrt(variance)
	// Guard against effectively-zero stddev. A constant return
	// series produces stddev=0 in math, but float64 log/division
	// rounding leaves residual noise (~1e-16) so an `== 0` check
	// would miss it and the resulting Sharpe explodes to ~1e16.
	// 1e-10 is well below any plausible daily-return stddev
	// (typical equity strategies sit in 0.005–0.05 range) and
	// catches numerical-precision-only stddev cleanly.
	if stddev < 1e-10 {
		return 0
	}
	// Annualise: 252 trading days/year. Risk-free rate is
	// zero by convention for this kind of intra-strategy
	// soak metric — the operator wants relative quality,
	// not the spread vs T-bills.
	return mean / stddev * math.Sqrt(252)
}

func maxDrawdownPct(snaps []*persistence.TradingPositionsSnapshot) float64 {
	if len(snaps) == 0 {
		return 0
	}
	hwm := 0.0
	maxDD := 0.0
	for _, s := range snaps {
		if s == nil || s.EquityUSD <= 0 {
			continue
		}
		if s.EquityUSD > hwm {
			hwm = s.EquityUSD
			continue
		}
		if hwm <= 0 {
			continue
		}
		dd := (hwm - s.EquityUSD) / hwm * 100
		if dd > maxDD {
			maxDD = dd
		}
	}
	return maxDD
}

// equity24hChange returns the 24h equity move as (pct, usd).
// Both default to 0 when the window can't be computed (no valid
// pair of snapshots straddling the 24h boundary). The dollar
// figure is surfaced alongside the percent because for small
// positions on a large equity base (e.g. a $30 move on a $1M
// account) the percent rounds to 0.00 with the panel's
// 2-decimal-place format and reads as "no movement at all".
func equity24hChange(snaps []*persistence.TradingPositionsSnapshot) (float64, float64) {
	if len(snaps) < 2 {
		return 0, 0
	}
	last := snaps[len(snaps)-1]
	if last == nil || last.EquityUSD <= 0 {
		return 0, 0
	}
	cutoff := last.RecordedAt.Add(-24 * time.Hour)
	// Find the OLDEST snapshot at or after cutoff. The
	// snapshot series is sorted ASC by RecordedAt so a
	// linear walk from the start works fine; binary search
	// would be marginally faster but at sampler defaults
	// (288 samples/day × 7 days = 2016 entries) the constant
	// factor is negligible.
	for _, s := range snaps {
		if s == nil || s.EquityUSD <= 0 {
			continue
		}
		if s.RecordedAt.Before(cutoff) {
			continue
		}
		delta := last.EquityUSD - s.EquityUSD
		return delta / s.EquityUSD * 100, delta
	}
	return 0, 0
}
