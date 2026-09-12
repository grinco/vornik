package trading

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// AnalysisEvidence configures the deterministic check that a trading
// strategist step actually examined every symbol it was responsible for.
// Design: https://docs.vornik.io §Analysis-
// evidence gate (2026-09-10). The check reads the step's own tool audit; it
// proves calls with named arguments happened and that carried numbers equal
// tool outputs, not that the agent reasoned about them.
type AnalysisEvidence struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MinDailyBars is what a get_historical_bars fallback must return for a
	// symbol to count as examined when no scorecard call covered it.
	MinDailyBars int `yaml:"min_daily_bars" json:"min_daily_bars"`
	// BenchmarkSymbols are examined every tick alongside held and entry-
	// universe symbols (the regime benchmark, SPY for ibkr-trader).
	BenchmarkSymbols []string `yaml:"benchmark_symbols" json:"benchmark_symbols"`
}

// Validate rejects settings that would turn the gate into a no-op silently.
func (a AnalysisEvidence) Validate() error {
	if a.MinDailyBars < 0 {
		return fmt.Errorf("min_daily_bars cannot be negative")
	}
	if a.Enabled && a.MinDailyBars == 0 {
		return fmt.Errorf("enabled analysis evidence requires a positive min_daily_bars")
	}
	return nil
}

// ToolCall is the projection of one tool_audit_log row the gate reads.
type ToolCall struct {
	Name   string
	Input  string
	Output string
	OK     bool
}

// EvidenceRefusal names why a step failed the gate and what was missing.
type EvidenceRefusal struct {
	Reason string
	Detail string
}

// Refusal reasons; they label vornik_trading_evidence_refused_total.
const (
	ReasonNoAccountSnapshot           = "no_account_snapshot"
	ReasonNoAnalysisCalls             = "no_analysis_calls"
	ReasonSymbolUnexamined            = "symbol_unexamined"
	ReasonHoldingsReviewMissing       = "holdings_review_missing"
	ReasonHoldingsReviewMismatch      = "holdings_review_mismatch"
	ReasonHoldingsUnavailableUnbacked = "holdings_review_unavailable_unbacked"
	ReasonExitRuleViolated            = "exit_rule_violated"
	ReasonProposalUnscored            = "proposal_unscored"
	ReasonProposalMismatch            = "proposal_mismatch"
	ReasonRegionUnscored              = "region_unscored"
)

// unavailableFallbackBars is the broker-sourced history a held symbol needs
// before an "unavailable" holdings verdict is accepted. It is deliberately
// lower than AnalysisEvidence.MinDailyBars (205, the scorecard's EMA200
// warm-up): the fallback only has to support the SMA50 exit rule the agent
// judged the position on, and raising it to 205 would turn every sidecar
// outage into an unexaminable book.
const unavailableFallbackBars = 50

// valueTolerance is half a unit of the fourth decimal: the sidecar rounds
// last_close and sma50 to four decimals, so a value copied verbatim differs
// by float parsing only, and anything that would round differently refuses.
const valueTolerance = 0.00005

type scorecardOut struct {
	Total, Trend, Momentum, Macro *int
	LastClose, SMA50              *float64
}

type regimeOut struct {
	Score *int
	Label string
}

type evidence struct {
	held       map[string]float64
	scorecards map[string]scorecardOut
	regimes    map[string]regimeOut
	barsDeep   map[string]int
	analysis   int
	snapshot   bool
}

func bareTool(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func upperField(raw, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

func collect(calls []ToolCall) evidence {
	ev := evidence{held: map[string]float64{}, scorecards: map[string]scorecardOut{}, regimes: map[string]regimeOut{}, barsDeep: map[string]int{}}
	for _, c := range calls {
		switch bareTool(c.Name) {
		case "get_account_summary":
			ev.collectSnapshot(c)
		case "scorecard":
			ev.analysis++
			ev.collectScorecard(c)
		case "regime":
			ev.collectRegime(c)
		case "get_historical_bars":
			ev.analysis++
			ev.collectBars(c)
		}
	}
	return ev
}

func (ev *evidence) collectSnapshot(c ToolCall) {
	if !c.OK {
		return
	}
	var out struct {
		Positions *[]struct {
			Symbol string  `json:"symbol"`
			Qty    float64 `json:"qty"`
		} `json:"positions"`
	}
	// A wrapped envelope or a response without a positions key is not a
	// snapshot: an unparsable book must not read as flat.
	if json.Unmarshal([]byte(c.Output), &out) != nil || out.Positions == nil {
		return
	}
	ev.snapshot = true
	ev.held = map[string]float64{}
	for _, p := range *out.Positions {
		if p.Qty != 0 {
			ev.held[strings.ToUpper(strings.TrimSpace(p.Symbol))] = p.Qty
		}
	}
}

func (ev *evidence) collectScorecard(c ToolCall) {
	sym := upperField(c.Input, "symbol")
	if sym == "" || !c.OK {
		return
	}
	var out struct {
		Total     *int     `json:"total"`
		Trend     *int     `json:"trend"`
		Momentum  *int     `json:"momentum"`
		Macro     *int     `json:"macro"`
		LastClose *float64 `json:"last_close"`
		SMA50     *float64 `json:"sma50"`
	}
	if json.Unmarshal([]byte(c.Output), &out) != nil || out.Total == nil {
		return
	}
	ev.scorecards[sym] = scorecardOut{out.Total, out.Trend, out.Momentum, out.Macro, out.LastClose, out.SMA50}
}

func (ev *evidence) collectRegime(c ToolCall) {
	if !c.OK {
		return
	}
	region := strings.ToLower(upperField(c.Input, "region"))
	var out struct {
		Region string `json:"region"`
		Score  *int   `json:"score"`
		Label  string `json:"label"`
	}
	if json.Unmarshal([]byte(c.Output), &out) != nil || out.Score == nil {
		return
	}
	if region == "" {
		region = strings.ToLower(out.Region)
	}
	ev.regimes[region] = regimeOut{out.Score, strings.ToUpper(out.Label)}
}

func (ev *evidence) collectBars(c ToolCall) {
	sym := upperField(c.Input, "symbol")
	if sym == "" || !c.OK {
		return
	}
	var out struct {
		Bars []json.RawMessage `json:"bars"`
	}
	if json.Unmarshal([]byte(c.Output), &out) != nil {
		return
	}
	if len(out.Bars) > ev.barsDeep[sym] {
		ev.barsDeep[sym] = len(out.Bars)
	}
}

type reviewEntry struct {
	Symbol    string   `json:"symbol"`
	LastClose *float64 `json:"last_close"`
	SMA50     *float64 `json:"sma50"`
	Verdict   string   `json:"verdict"`
}

type evidenceProposal struct {
	Symbol    string `json:"symbol"`
	Intent    string `json:"intent"`
	Region    string `json:"region"`
	Scorecard struct {
		Total, Trend, Momentum, Macro *int
	} `json:"scorecard"`
	Regime struct {
		Score *int   `json:"score"`
		Label string `json:"label"`
	} `json:"regime"`
}

func near(a, b *float64) bool {
	return a != nil && b != nil && math.Abs(*a-*b) <= valueTolerance
}

func sameInt(a, b *int) bool { return a != nil && b != nil && *a == *b }

// CheckAnalysisEvidence returns nil when the step's tool calls cover every
// symbol in benchmark ∪ allowed ∪ held and the result's holdings_review and
// open proposals carry values the tools returned. result is the strategist
// envelope; a result without a proposals key is not a strategist step and
// passes. protected symbols are never expected to close.
func CheckAnalysisEvidence(result []byte, calls []ToolCall, cfg AnalysisEvidence, allowed, protected []string) *EvidenceRefusal {
	if !cfg.Enabled {
		return nil
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(result, &env) != nil || env == nil {
		return nil
	}
	rawProposals, ok := env["proposals"]
	if !ok {
		return nil
	}
	ev := collect(calls)
	if !ev.snapshot {
		return &EvidenceRefusal{ReasonNoAccountSnapshot, "no successful get_account_summary call recorded in this step; holdings unknown"}
	}
	if ev.analysis == 0 {
		return &EvidenceRefusal{ReasonNoAnalysisCalls, "no mcp__ta__scorecard or get_historical_bars call recorded in this step; nothing was examined"}
	}
	if missing := ev.unexamined(cfg, allowed); len(missing) > 0 {
		return &EvidenceRefusal{ReasonSymbolUnexamined, fmt.Sprintf("symbols not examined this step: %s (each needs a successful mcp__ta__scorecard call with that symbol, or get_historical_bars returning at least %d daily bars)", strings.Join(missing, ", "), cfg.MinDailyBars)}
	}
	var proposals []evidenceProposal
	if err := json.Unmarshal(rawProposals, &proposals); err != nil {
		return &EvidenceRefusal{ReasonProposalMismatch, "proposals is not an array of objects"}
	}
	closes := map[string]bool{}
	for _, p := range proposals {
		if strings.EqualFold(strings.TrimSpace(p.Intent), "close") {
			closes[strings.ToUpper(strings.TrimSpace(p.Symbol))] = true
		}
	}
	isProtected := map[string]bool{}
	for _, s := range protected {
		isProtected[strings.ToUpper(strings.TrimSpace(s))] = true
	}
	if r := checkHoldingsReview(env, ev, closes, isProtected); r != nil {
		return r
	}
	return checkOpenProposals(proposals, ev)
}

// unexamined lists the non-held required symbols (benchmark ∪ allowed) with
// neither a scorecard call nor a deep-enough bars fetch; held symbols are
// judged by the holdings review instead.
func (ev *evidence) unexamined(cfg AnalysisEvidence, allowed []string) []string {
	required := map[string]bool{}
	for _, s := range append(append([]string{}, cfg.BenchmarkSymbols...), allowed...) {
		required[strings.ToUpper(strings.TrimSpace(s))] = true
	}
	delete(required, "")
	var missing []string
	for s := range required {
		if _, isHeld := ev.held[s]; isHeld {
			continue
		}
		if _, scored := ev.scorecards[s]; scored || ev.barsDeep[s] >= cfg.MinDailyBars {
			continue
		}
		missing = append(missing, s)
	}
	sort.Strings(missing)
	return missing
}

func checkOpenProposals(proposals []evidenceProposal, ev evidence) *EvidenceRefusal {
	for _, p := range proposals {
		if strings.EqualFold(strings.TrimSpace(p.Intent), "close") {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
		scored, ok := ev.scorecards[sym]
		if !ok {
			return &EvidenceRefusal{ReasonProposalUnscored, "open proposal " + sym + " has no successful mcp__ta__scorecard call for that symbol in this step"}
		}
		if !sameInt(p.Scorecard.Total, scored.Total) || !sameInt(p.Scorecard.Trend, scored.Trend) ||
			!sameInt(p.Scorecard.Momentum, scored.Momentum) || !sameInt(p.Scorecard.Macro, scored.Macro) {
			return &EvidenceRefusal{ReasonProposalMismatch, "open proposal " + sym + " carries scorecard values that differ from the mcp__ta__scorecard output for " + sym}
		}
		region := strings.ToLower(strings.TrimSpace(p.Region))
		reg, ok := ev.regimes[region]
		if !ok {
			return &EvidenceRefusal{ReasonRegionUnscored, fmt.Sprintf("open proposal %s names region %q but no successful mcp__ta__regime call for that region was recorded this step", sym, region)}
		}
		if !sameInt(p.Regime.Score, reg.Score) || !strings.EqualFold(strings.TrimSpace(p.Regime.Label), reg.Label) {
			return &EvidenceRefusal{ReasonProposalMismatch, "open proposal " + sym + " carries regime values that differ from the mcp__ta__regime output for " + region}
		}
	}
	return nil
}

func checkHoldingsReview(env map[string]json.RawMessage, ev evidence, closes, isProtected map[string]bool) *EvidenceRefusal {
	rawReview, ok := env["holdings_review"]
	if !ok {
		if len(ev.held) == 0 {
			return nil
		}
		return &EvidenceRefusal{ReasonHoldingsReviewMissing, "holdings_review is required: one entry per held symbol with last_close, sma50 and verdict"}
	}
	var review []reviewEntry
	if json.Unmarshal(rawReview, &review) != nil {
		return &EvidenceRefusal{ReasonHoldingsReviewMissing, "holdings_review is not an array of {symbol,last_close,sma50,verdict}"}
	}
	byHeld := map[string]reviewEntry{}
	for _, r := range review {
		byHeld[strings.ToUpper(strings.TrimSpace(r.Symbol))] = r
	}
	symbols := make([]string, 0, len(ev.held))
	for s := range ev.held {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	for _, sym := range symbols {
		entry, ok := byHeld[sym]
		if !ok {
			return &EvidenceRefusal{ReasonHoldingsReviewMissing, "holdings_review has no entry for held symbol " + sym}
		}
		if r := checkOneHolding(sym, entry, ev, closes[sym], isProtected[sym]); r != nil {
			return r
		}
	}
	return nil
}

func checkOneHolding(sym string, entry reviewEntry, ev evidence, hasClose, protected bool) *EvidenceRefusal {
	verdict := strings.ToLower(strings.TrimSpace(entry.Verdict))
	scored, hasScore := ev.scorecards[sym]
	if verdict == "unavailable" {
		if hasScore {
			return &EvidenceRefusal{ReasonHoldingsReviewMismatch, "holdings_review marks " + sym + " unavailable but mcp__ta__scorecard succeeded for it this step"}
		}
		if ev.barsDeep[sym] < unavailableFallbackBars {
			return &EvidenceRefusal{ReasonHoldingsUnavailableUnbacked, fmt.Sprintf("holdings_review marks %s unavailable without a successful get_historical_bars call returning at least %d daily bars to judge the exit on", sym, unavailableFallbackBars)}
		}
		return nil
	}
	if !hasScore || scored.LastClose == nil || scored.SMA50 == nil {
		return &EvidenceRefusal{ReasonHoldingsUnavailableUnbacked, "held symbol " + sym + " has no successful mcp__ta__scorecard output this step; list it with verdict unavailable and fetch its broker bars, or examine it"}
	}
	if !near(entry.LastClose, scored.LastClose) || !near(entry.SMA50, scored.SMA50) {
		return &EvidenceRefusal{ReasonHoldingsReviewMismatch, "holdings_review last_close/sma50 for " + sym + " differ from the mcp__ta__scorecard output"}
	}
	expected := "hold"
	if *scored.LastClose < *scored.SMA50 && !protected {
		expected = "close"
	}
	if verdict != expected {
		return &EvidenceRefusal{ReasonExitRuleViolated, fmt.Sprintf("held symbol %s: last_close %.4f vs sma50 %.4f requires verdict %s, got %q", sym, *scored.LastClose, *scored.SMA50, expected, entry.Verdict)}
	}
	if expected == "close" && !hasClose {
		return &EvidenceRefusal{ReasonExitRuleViolated, "held symbol " + sym + " has verdict close but no intent=close proposal"}
	}
	return nil
}
