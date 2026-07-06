package verifier

import (
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

// evaluateFloorProposal applies the entry floor and protected-symbol
// EXIT guard to a single proposal, returning the first Violation it
// produces (and incrementing the rejection metric) or nil when the
// proposal clears both. Split out of verifyScorecardFloor to keep that
// function within the funlen budget.
func evaluateFloorProposal(cfg Config, p floorProposal, entryCfg trading.EntryConfig, protected map[string]bool) *Violation {
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
	// The real proposal schema carries no `side`; derive the floor's
	// entry side from the authoritative intent+action pair. Only an OPEN
	// maps to a gated side (BUY→long, SELL→short); a close (or any other
	// intent) maps to "" so trading.EvaluateEntry treats it as a non-open
	// and waves it through — closes are never floor-gated. Normalize
	// casing fail-closed (an LLM emitting "Open"/"buy" must still gate).
	intent := strings.ToLower(strings.TrimSpace(p.Intent))
	action := strings.ToUpper(strings.TrimSpace(p.Action))
	var side string
	switch {
	case intent == "open" && action == "BUY":
		side = "long"
	case intent == "open" && action == "SELL":
		side = "short"
	default:
		side = "" // close / unknown ⇒ non-open, not floor-gated
	}
	// An OPEN whose action didn't normalize to BUY/SELL (e.g. "HOLD", an
	// empty string, or "LONG") derives side=="" just like a close does —
	// but it is NOT a close, so waving it through here would let a
	// malformed/hallucinated open bypass the entire fail-closed floor.
	// Reject it explicitly rather than letting the side=="" branch below
	// treat it as a non-open.
	if side == "" && intent == "open" {
		incFloorRejected("unknown_action")
		return &Violation{
			VerifierName: nameOrDefault(cfg, "scorecard_floor"),
			Type:         cfg.Type,
			Detail: fmt.Sprintf(
				"strategist proposed a %s open on %s that fails the code-enforced entry floor: unknown_action (action=%q) — only BUY/SELL opens are floor-gated, so an unrecognized action must not bypass the deterministic gate",
				p.Intent, p.Symbol, p.Action,
			),
		}
	}
	// Normalize the free-form regime label and region to the canonical
	// casing trading.EvaluateEntry compares against, or the gate fails
	// OPEN: it matches RegimeLabel against the UPPERCASE literal
	// "RISK_OFF" and keys MinComponentCount by the lowercase region
	// ("us"/"eu"/"apac"). An LLM emitting "risk_off" or "US" would
	// otherwise slip the risk_off_long and panel_incomplete blocks — the
	// exact inverse of this fail-closed gate's purpose.
	label = strings.ToUpper(strings.TrimSpace(label))
	region := strings.ToLower(strings.TrimSpace(p.Region))

	// Entry floor: only OPEN sides are gated (EvaluateEntry waves
	// through non-opens). First failure wins.
	ok, reason := trading.EvaluateEntry(trading.EntryProposal{
		Symbol:         p.Symbol,
		Side:           side,
		Region:         region,
		RegimeLabel:    label,
		Total:          total,
		ComponentCount: componentCount,
		RegimeStale:    stale,
		HasScores:      hasScores,
	}, entryCfg)
	if !ok {
		incFloorRejected(reason)
		return &Violation{
			VerifierName: nameOrDefault(cfg, "scorecard_floor"),
			Type:         cfg.Type,
			Detail: fmt.Sprintf(
				"strategist proposed a %s open on %s that fails the code-enforced entry floor: %s (total=%d, regime=%s, stale=%t, components=%d) — the LLM narrative can't override the deterministic gate",
				side, p.Symbol, reason, total, label, stale, componentCount,
			),
		}
	}

	// Protected-symbol close guard: intent=="close" is the authoritative
	// "this is an exit" signal from the executor/broker schema — the
	// strategist must never flatten a symbol the operator holds outside
	// the strategy's model. This keys off intent (NOT the trading.Classify
	// cascade): Classify can return a non-exit action for a close
	// proposal, so using it here would be a bug. The internal/trading
	// cascade classifier stays as a tested roadmap primitive but is
	// intentionally NOT in the v1 enforcement path.
	if intent == "close" && protected[strings.ToUpper(strings.TrimSpace(p.Symbol))] {
		incFloorRejected("protected_symbol")
		return &Violation{
			VerifierName: nameOrDefault(cfg, "scorecard_floor"),
			Type:         cfg.Type,
			Detail: fmt.Sprintf(
				"strategist proposed a close (intent=close) on protected symbol %s — protected_symbol positions are held outside the strategy's model and must never be closed/reduced by it",
				p.Symbol,
			),
		}
	}
	return nil
}
