package verifier

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/trading"
)

// TradingFloorConfig is the projection of a project's scorecard/regime
// entry-floor configuration (registry Project.Trading) that the
// scorecard_floor verifier needs. The executor's verifier call site
// populates it for trading projects; nil for non-trading projects,
// which makes scorecard_floor a clean no-op. Mirrors the projected
// shape used for WatchlistAllowList so the verifier package doesn't
// take a dependency on the registry package.
type TradingFloorConfig struct {
	// ScorecardEnabled and RegimeEnabled are the operator opt-in
	// flags. The floor is enforced only when BOTH are true (the soak
	// gate); otherwise the verifier is inert.
	ScorecardEnabled bool
	RegimeEnabled    bool
	// MinEntryTotal is the minimum composite scorecard total a long/
	// short open must reach.
	MinEntryTotal int
	// BlockLongInRiskOff refuses long opens while the regime reads
	// RISK_OFF.
	BlockLongInRiskOff bool
	// StaleBehavior governs a stale regime read: "block_opens"
	// (default, fail-closed), "neutral", or "last_known" (the two
	// opt-outs that let a stale open through).
	StaleBehavior string
	// MinComponentCount is the per-region breadth-component floor for
	// the regime read to count as valid rather than degraded.
	MinComponentCount map[string]int
	// ProtectedSymbols are tickers whose EXIT-class actions the
	// verifier refuses regardless of scores.
	ProtectedSymbols []string
}

// floorRejectedTotal is the vornik_trading_floor_rejected_total counter,
// nil until RegisterFloorMetrics wires it onto the SERVED observability
// registry. It is deliberately NOT created via promauto.NewCounterVec
// (that lands on prometheus.DefaultRegisterer, which production never
// serves — the 2026-06-06 invisible-metric class). Nil-safe: when the
// wiring pass didn't run (CE builds, unit tests) increments no-op.
var floorRejectedTotal *prometheus.CounterVec

// RegisterFloorMetrics registers vornik_trading_floor_rejected_total on
// the given Prometheus registerer (the served observability registry)
// and caches it for the scorecard_floor verifier to increment. Call it
// once, at the same wiring pass where the executor's metrics are
// registered (Container.rebuildSchedulerMetrics), so the counter is
// visible on /metrics rather than orphaned on the default registry.
//
// Labelled by the floor rejection reason (below_min, stale,
// risk_off_long, panel_incomplete, missing_scores, protected_symbol,
// unknown_action) so dashboards can attribute strategist rejections to
// their cause. Safe to leave uncalled: increments are nil-guarded (CE /
// tests observe no metric, not a panic).
func RegisterFloorMetrics(reg prometheus.Registerer) {
	if reg == nil {
		return
	}
	floorRejectedTotal = promauto.With(reg).NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "trading",
			Name:      "floor_rejected_total",
			Help:      "Count of strategist proposals rejected by the code-enforced entry floor (scorecard_floor verifier), labelled by rejection reason.",
		},
		[]string{"reason"},
	)
}

// incFloorRejected increments the floor-rejection counter for reason,
// nil-safe so verifier logic (and its unit tests) run whether or not
// RegisterFloorMetrics was wired.
func incFloorRejected(reason string) {
	if floorRejectedTotal != nil {
		floorRejectedTotal.WithLabelValues(reason).Inc()
	}
}

// floorProposal is the projection of a strategist proposal the
// scorecard_floor verifier reads: the carried scorecard + regime
// snapshot plus the authoritative intent/action the floor keys off.
// The real (executor/broker-authoritative) proposal schema carries no
// `side`; it is {symbol, intent: open|close, action: BUY|SELL, ...}.
// The floor derives an entry "side" from intent+action, and the
// protected-symbol guard keys off intent=="close" — the authoritative
// "this is an exit" signal (see evaluateFloorProposal). Scorecard and
// Regime are pointers so an absent object (rather than a zero-valued
// one) is distinguishable — that's the fail-closed missing_scores
// signal. HoldingState is parsed but not consumed by the v1
// enforcement path (Task 10 emits it; the trading.Classify cascade that
// would use it is intentionally not wired into enforcement in v1).
type floorProposal struct {
	Symbol       string `json:"symbol"`
	Intent       string `json:"intent"`
	Action       string `json:"action"`
	HoldingState string `json:"holding_state"`
	Region       string `json:"region"`
	Scorecard    *struct {
		Total    int `json:"total"`
		Trend    int `json:"trend"`
		Momentum int `json:"momentum"`
		Macro    int `json:"macro"`
	} `json:"scorecard"`
	Regime *struct {
		Score          int    `json:"score"`
		Label          string `json:"label"`
		Stale          bool   `json:"stale"`
		ComponentCount int    `json:"component_count"`
	} `json:"regime"`
}

// extractFloorProposals parses the strategist's proposals[] with the
// same envelope-merge fallback as extractProposals (the agent harness
// occasionally leaves the structured output nested inside a `message`
// JSON-string). Returns nil on any parse failure so the verifier
// no-ops rather than block on malformed output.
func extractFloorProposals(resultBytes []byte) []floorProposal {
	var envelope struct {
		Proposals []floorProposal `json:"proposals"`
		Message   string          `json:"message"`
	}
	if err := jsonUnmarshalLenient(resultBytes, &envelope); err != nil {
		return nil
	}
	if len(envelope.Proposals) > 0 {
		return envelope.Proposals
	}
	if envelope.Message != "" {
		var inner struct {
			Proposals []floorProposal `json:"proposals"`
		}
		if err := jsonUnmarshalLenient([]byte(envelope.Message), &inner); err == nil {
			return inner.Proposals
		}
	}
	return nil
}

// verifyScorecardFloor — fail the strategist step when any OPEN
// proposal fails the code-enforced entry floor (trading.EvaluateEntry),
// or when a close (intent=="close") targets a protected symbol. This is
// the code-side backstop for the scorecard/regime gate: the strategist
// emits the scores + a narrative, and this verifier — not the LLM —
// decides whether the open clears the floor.
//
// Inert unless BOTH gates are enabled (the soak gate): a project can
// carry the config with the flags off and see zero behaviour change.
// On the first failing proposal it increments
// vornik_trading_floor_rejected_total{reason} and rejects, naming the
// symbol and reason. Fail-closed on absent scores (missing_scores).
func verifyScorecardFloor(cfg Config, in Input) *Violation {
	fc := in.TradingFloor
	if fc == nil || !fc.ScorecardEnabled || !fc.RegimeEnabled || len(in.ResultJSON) == 0 {
		return nil
	}
	proposals := extractFloorProposals(in.ResultJSON)
	if len(proposals) == 0 {
		return nil
	}

	protected := make(map[string]bool, len(fc.ProtectedSymbols))
	for _, sym := range fc.ProtectedSymbols {
		protected[strings.ToUpper(strings.TrimSpace(sym))] = true
	}
	entryCfg := trading.EntryConfig{
		MinEntryTotal:      fc.MinEntryTotal,
		BlockLongInRiskOff: fc.BlockLongInRiskOff,
		StaleBehavior:      fc.StaleBehavior,
		MinComponentCount:  fc.MinComponentCount,
		Protected:          protected,
	}

	for _, p := range proposals {
		if v := evaluateFloorProposal(cfg, p, entryCfg, protected); v != nil {
			return v
		}
	}
	return nil
}

// floorVerdict is the disposition classifyFloorProposal assigns to one
// proposal: keep it, soft-DROP it (a policy floor rejection — the tick still
// proceeds), or HARD-fail the step (an integrity violation).
type floorVerdict int

const (
	floorKeep floorVerdict = iota
	floorDrop
	floorHard
)

// classifyFloorProposal is the shared per-proposal decision used by BOTH the
// scorecard_floor verifier (→ Violation) and FilterTradingProposals (→ drop/
// hard). It applies the code-enforced entry floor + the protected-symbol EXIT
// guard and returns the verdict, the reason label (for the metric), and a
// human-readable detail. Pure: no metric/side effects (callers increment).
//
// Split (soft-drop design, 2026-07-25): floor policy rejections on OPENS
// (below_min / missing_scores / stale / panel_incomplete / risk_off_long) are
// DROP (the open is removed, the tick NO_ACTIONs); integrity violations
// (unknown_action on an open, protected_symbol close) are HARD. A protected
// symbol OPEN is NOT hard — it flows through the normal floor (keep if it
// clears, drop if sub-floor); only closing/reducing a protected symbol is hard.
func classifyFloorProposal(p floorProposal, entryCfg trading.EntryConfig, protected map[string]bool) (floorVerdict, string, string) {
	hasScores := p.Scorecard != nil && p.Regime != nil
	var total, componentCount int
	var label string
	var stale bool
	if hasScores {
		total = p.Scorecard.Total
		label = p.Regime.Label
		stale = p.Regime.Stale
		componentCount = p.Regime.ComponentCount
	}
	intent := strings.ToLower(strings.TrimSpace(p.Intent))
	action := strings.ToUpper(strings.TrimSpace(p.Action))
	var side string
	switch {
	case intent == "open" && action == "BUY":
		side = "long"
	case intent == "open" && action == "SELL":
		side = "short"
	default:
		side = ""
	}
	// Malformed OPEN (action not BUY/SELL) — integrity violation, HARD (never
	// silently waved through as a non-open).
	if side == "" && intent == "open" {
		return floorHard, "unknown_action", fmt.Sprintf(
			"strategist proposed an open on %s that fails the code-enforced entry floor: unknown_action (action=%q) — only BUY/SELL opens are floor-gated, so an unrecognized action must not bypass the deterministic gate",
			p.Symbol, p.Action)
	}
	label = strings.ToUpper(strings.TrimSpace(label))
	region := strings.ToLower(strings.TrimSpace(p.Region))

	// Entry floor: only OPEN sides gated (EvaluateEntry waves through non-opens).
	// A floor failure on an OPEN is a soft DROP.
	ok, reason := trading.EvaluateEntry(trading.EntryProposal{
		Symbol: p.Symbol, Side: side, Region: region, RegimeLabel: label,
		Total: total, ComponentCount: componentCount, RegimeStale: stale, HasScores: hasScores,
	}, entryCfg)
	if !ok {
		return floorDrop, reason, fmt.Sprintf(
			"dropped %s open on %s: %s (total=%d, regime=%s, stale=%t, components=%d) — below the code-enforced entry floor",
			side, p.Symbol, reason, total, label, stale, componentCount)
	}

	// Protected-symbol CLOSE guard — integrity violation, HARD. (A protected
	// OPEN already passed the floor above and is kept; only reducing a
	// protected holding is refused.)
	if intent == "close" && protected[strings.ToUpper(strings.TrimSpace(p.Symbol))] {
		return floorHard, "protected_symbol", fmt.Sprintf(
			"strategist proposed a close (intent=close) on protected symbol %s — protected_symbol positions are held outside the strategy's model and must never be closed/reduced by it",
			p.Symbol)
	}
	return floorKeep, "", ""
}

// evaluateFloorProposal applies the entry floor and protected-symbol
// EXIT guard to a single proposal, returning the first Violation it
// produces (and incrementing the rejection metric) or nil when the
// proposal clears both. Split out of verifyScorecardFloor to keep that
// function within the funlen budget. Delegates the decision to the shared
// classifyFloorProposal so the verifier + FilterTradingProposals agree.
func evaluateFloorProposal(cfg Config, p floorProposal, entryCfg trading.EntryConfig, protected map[string]bool) *Violation {
	verdict, reason, detail := classifyFloorProposal(p, entryCfg, protected)
	if verdict == floorKeep {
		return nil
	}
	incFloorRejected(reason)
	return &Violation{
		VerifierName: nameOrDefault(cfg, "scorecard_floor"),
		Type:         cfg.Type,
		Detail:       detail,
	}
}

// FilterTradingProposals is the soft-drop enforcement seam (design
// 2026-07-25-scorecard-floor-soft-drop): given the strategist step's raw
// result.json, it DROPS floor-failing OPEN proposals (removing whole entries,
// preserving each kept entry's full fields) so they never reach the risk-officer
// / executor / place_order, increments vornik_trading_floor_rejected_total per
// drop, and returns the rewritten bytes. It returns a non-nil error (HARD fail —
// the caller fails the step) on an integrity violation (protected-symbol close,
// unknown action) or an unparseable proposal (fail-closed). No-op (returns the
// input unchanged) when the floor is disabled, there are no proposals, or the
// output can't be parsed as an envelope.
//
// This is the ENFORCEMENT path; the scorecard_floor verifier is demoted to
// SeverityWarn and runs AFTER this filter as an observability backstop on the
// already-filtered set (design D3).
func FilterTradingProposals(resultBytes []byte, fc *TradingFloorConfig) ([]byte, error) {
	if fc == nil || !fc.ScorecardEnabled || !fc.RegimeEnabled || len(resultBytes) == 0 {
		return resultBytes, nil
	}
	protected := make(map[string]bool, len(fc.ProtectedSymbols))
	for _, sym := range fc.ProtectedSymbols {
		protected[strings.ToUpper(strings.TrimSpace(sym))] = true
	}
	entryCfg := trading.EntryConfig{
		MinEntryTotal:      fc.MinEntryTotal,
		BlockLongInRiskOff: fc.BlockLongInRiskOff,
		StaleBehavior:      fc.StaleBehavior,
		MinComponentCount:  fc.MinComponentCount,
		Protected:          protected,
	}

	// Parse into an ordered-agnostic object so every top-level field the
	// executor/risk-officer read (rationale, has_proposals, …) survives; only
	// proposals[] is rewritten. A parse failure → no-op (verifier still warns).
	var obj map[string]json.RawMessage
	if err := jsonUnmarshalLenient(resultBytes, &obj); err != nil || obj == nil {
		return resultBytes, nil
	}
	rawProps, ok := obj["proposals"]
	if !ok {
		return resultBytes, nil // no proposals key → nothing to filter
	}
	var proposals []json.RawMessage
	if err := json.Unmarshal(rawProps, &proposals); err != nil || len(proposals) == 0 {
		return resultBytes, nil // empty/absent proposals → no-op (no panic)
	}

	kept, dropped, hardErr := filterProposalsRaw(proposals, entryCfg, protected)
	if hardErr != nil {
		return resultBytes, hardErr // integrity/parse violation → fail the step (bytes unchanged)
	}
	if dropped == 0 {
		return resultBytes, nil // nothing dropped → return input verbatim
	}
	// Rewrite proposals[] with the kept entries + recompute has_proposals so a
	// no-remaining-proposal tick reads has_proposals=false (consistency; the
	// maybe_execute gate keys off the risk-officer's has_approvals, D5).
	// Fail-CLOSED on a re-marshal error (review-20260724-98de #2): we already
	// decided to drop opens, so returning the ORIGINAL (unfiltered) bytes would
	// let the sub-floor opens through — the exact inverse of the gate. A marshal
	// failure over verbatim RawMessages is near-impossible, but if it happens we
	// fail the step rather than silently un-drop.
	keptBytes, err := json.Marshal(kept)
	if err != nil {
		return resultBytes, fmt.Errorf("scorecard_floor: re-marshal of filtered proposals failed (fail-closed, %d dropped): %w", dropped, err)
	}
	obj["proposals"] = keptBytes
	obj["has_proposals"] = json.RawMessage(map[bool]string{true: "true", false: "false"}[len(kept) > 0])
	out, err := json.Marshal(obj)
	if err != nil {
		return resultBytes, fmt.Errorf("scorecard_floor: re-marshal of the filtered envelope failed (fail-closed, %d dropped): %w", dropped, err)
	}
	return out, nil
}

// filterProposalsRaw classifies each proposal (preserving kept entries' full
// JSON verbatim), returning the kept RawMessages, the soft-drop count, and the
// first HARD error. An unparseable proposal is a HARD error (fail-closed: never
// keep an entry we can't classify).
func filterProposalsRaw(raw []json.RawMessage, entryCfg trading.EntryConfig, protected map[string]bool) ([]json.RawMessage, int, error) {
	kept := make([]json.RawMessage, 0, len(raw))
	for _, rp := range raw {
		var p floorProposal
		if err := json.Unmarshal(rp, &p); err != nil {
			return nil, 0, fmt.Errorf("scorecard_floor: unparseable proposal (fail-closed): %w", err)
		}
		verdict, reason, detail := classifyFloorProposal(p, entryCfg, protected)
		switch verdict {
		case floorKeep:
			kept = append(kept, rp)
		case floorDrop:
			incFloorRejected(reason)
		case floorHard:
			// Single increment: the executor returns this error BEFORE runVerifiers
			// (container.go), so the (warn) verifier does NOT also run/count on a
			// hard-fail; on the soft path the verifier sees the already-filtered set
			// (no re-count). review-20260724-98de #1.
			incFloorRejected(reason)
			return nil, 0, fmt.Errorf("scorecard_floor: %s", detail)
		}
	}
	return kept, len(raw) - len(kept), nil
}
