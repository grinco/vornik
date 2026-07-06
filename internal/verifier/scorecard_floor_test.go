package verifier

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// floorCfgEnabled is the operator config with both gates live: a
// min-3 scorecard, block-long-in-risk-off, fail-closed staleness, a
// per-region breadth floor, and one protected symbol.
func floorCfgEnabled() *TradingFloorConfig {
	return &TradingFloorConfig{
		ScorecardEnabled:   true,
		RegimeEnabled:      true,
		MinEntryTotal:      3,
		BlockLongInRiskOff: true,
		StaleBehavior:      "block_opens",
		MinComponentCount:  map[string]int{"us": 3},
		ProtectedSymbols:   []string{"RSU-LOCKUP"},
	}
}

// TestScorecardFloor_InertWhenFlagsOff — the soak gate. With either
// gate disabled the verifier must PASS even a plainly below-floor
// open, so the strategist's existing entry path is untouched until an
// operator opts in.
func TestScorecardFloor_InertWhenFlagsOff(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	floor := floorCfgEnabled()
	floor.ScorecardEnabled = false // one flag off ⇒ inert
	in := Input{
		TradingFloor: floor,
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":2},
			 "regime":{"label":"RISK_ON","component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "verifier must be inert when a gate flag is off")
}

// TestScorecardFloor_InertWhenNoConfig — non-trading projects carry no
// floor config at all; the verifier must no-op.
func TestScorecardFloor_InertWhenNoConfig(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		ResultJSON: []byte(`{"proposals":[{"symbol":"AAPL","intent":"open","action":"BUY","scorecard":{"total":0},"regime":{}}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v)
}

// TestScorecardFloor_BelowMinRejects — a long open (intent=open,
// action=BUY) scoring under the operator's MinEntryTotal is rejected
// with below_min.
func TestScorecardFloor_BelowMinRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":2,"trend":1,"momentum":1},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "below-min open must be rejected")
	assert.Contains(t, v.Detail, "below_min")
	assert.Contains(t, v.Detail, "AAPL")
}

// TestScorecardFloor_RiskOffLongRejects — a qualifying long open
// (open+BUY) is still blocked when the regime reads RISK_OFF and the
// operator set block_long_in_risk_off.
func TestScorecardFloor_RiskOffLongRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"MSFT","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":4},
			 "regime":{"label":"RISK_OFF","stale":false,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "risk_off_long")
	assert.Contains(t, v.Detail, "MSFT")
}

// TestScorecardFloor_ShortOpenInRiskOffPasses — the risk-off guard is
// long-only: a SHORT open (open+SELL) into RISK_OFF must NOT trip
// risk_off_long. Everything else qualifies so this isolates the
// side derivation (SELL→short, not long).
func TestScorecardFloor_ShortOpenInRiskOffPasses(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"MSFT","intent":"open","action":"SELL","region":"us",
			 "scorecard":{"total":4},
			 "regime":{"label":"RISK_OFF","stale":false,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "a short open into RISK_OFF must not be blocked by the long-only risk-off guard")
}

// TestScorecardFloor_StaleRejects — fail-closed staleness blocks the
// open when StaleBehavior is the default block_opens.
func TestScorecardFloor_StaleRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"TSLA","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":5},
			 "regime":{"label":"RISK_ON","stale":true,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "stale")
	assert.Contains(t, v.Detail, "TSLA")
}

// TestScorecardFloor_PanelIncompleteRejects — a degraded regime panel
// (fewer breadth components than MinComponentCount for the region)
// blocks the open.
func TestScorecardFloor_PanelIncompleteRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":5},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":1}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "panel_incomplete")
}

// TestScorecardFloor_MissingScoresFailClosed — a long open carrying no
// scorecard/regime objects is rejected with missing_scores rather than
// waved through.
func TestScorecardFloor_MissingScoresFailClosed(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON:   []byte(`{"proposals":[{"symbol":"NVDA","intent":"open","action":"BUY","region":"us"}]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "missing_scores")
	assert.Contains(t, v.Detail, "NVDA")
}

// TestScorecardFloor_ProtectedSymbolCloseRejects — a close (intent=close)
// against a protected symbol is refused with protected_symbol even
// though it isn't an open. The guard keys off intent=close, the
// authoritative exit signal — NOT the trading.Classify cascade.
func TestScorecardFloor_ProtectedSymbolCloseRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"RSU-LOCKUP","intent":"close","action":"SELL","holding_state":"held","region":"us"}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)
	assert.Contains(t, v.Detail, "protected_symbol")
	assert.Contains(t, v.Detail, "RSU-LOCKUP")
}

// TestScorecardFloor_CloseOnNonProtectedPasses — a close on a symbol
// that is NOT protected passes: closes are never floor-gated.
func TestScorecardFloor_CloseOnNonProtectedPasses(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"close","action":"SELL","holding_state":"held","region":"us"}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "a close on a non-protected symbol must pass — closes are never floor-gated")
}

// TestScorecardFloor_RiskOffLongLowercaseRejects — C2 regression
// (fail-open casing). A qualifying LONG open whose regime.label the LLM
// emitted lowercase ("risk_off") must STILL be blocked with
// risk_off_long. Before the casing normalization the raw "risk_off"
// never equalled trading's canonical "RISK_OFF" literal, so the block
// silently failed OPEN — the opposite of this gate's purpose. Score is
// above min and the panel is full so the ONLY thing that can reject is
// the risk-off guard, isolating the casing bug.
func TestScorecardFloor_RiskOffLongLowercaseRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"MSFT","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":4},
			 "regime":{"label":"risk_off","stale":false,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "lowercase risk_off must not slip the risk_off_long block (fail-closed)")
	assert.Contains(t, v.Detail, "risk_off_long")
	assert.Contains(t, v.Detail, "MSFT")
}

// TestScorecardFloor_PanelIncompleteMixedCaseRegionRejects — C2
// regression (fail-open casing). MinComponentCount is keyed by the
// lowercase region ("us"), but the LLM emitted the region uppercase
// ("US"). Before region normalization the map lookup missed, so the
// degraded-panel open slipped the panel_incomplete gate (fail OPEN).
// Score is above min and regime is RISK_ON so panel_incomplete is the
// only reason that can fire.
func TestScorecardFloor_PanelIncompleteMixedCaseRegionRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"US",
			 "scorecard":{"total":5},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":1}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "uppercase region must still resolve the per-region breadth floor")
	assert.Contains(t, v.Detail, "panel_incomplete")
}

// TestScorecardFloor_CleanProposalPasses — a fully-qualifying long open
// (open+BUY, score above min, RISK_ON, full panel, fresh) passes.
func TestScorecardFloor_CleanProposalPasses(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":4,"trend":2,"momentum":2},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":5}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	assert.Nil(t, v, "a fully-qualifying open must pass")
}

// TestScorecardFloor_MetricIncrementsOnRejection — C1 wiring. After
// RegisterFloorMetrics wires the counter onto a (test) registry, a
// rejection must increment vornik_trading_floor_rejected_total for its
// reason label. Guards the served-registry plumbing end-to-end: before
// the fix the counter lived on the default registerer and was invisible
// to the served /metrics handler.
func TestScorecardFloor_MetricIncrementsOnRejection(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterFloorMetrics(reg)
	t.Cleanup(func() { floorRejectedTotal = nil }) // don't leak into sibling tests

	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":2},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":3}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v)

	got := testutil.ToFloat64(floorRejectedTotal.WithLabelValues("below_min"))
	assert.Equal(t, 1.0, got, "below_min rejection must increment the served counter")
}

// TestScorecardFloor_MetricNilSafeWhenUnregistered — increments must be
// a no-op (no panic) when RegisterFloorMetrics was never called, the CE
// / unit-test path.
func TestScorecardFloor_MetricNilSafeWhenUnregistered(t *testing.T) {
	floorRejectedTotal = nil // ensure unwired
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"BUY","region":"us",
			 "scorecard":{"total":1},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":3}}
		]}`),
	}
	require.NotPanics(t, func() {
		v, err := Run(context.Background(), cfg, in)
		require.NoError(t, err)
		require.NotNil(t, v)
	})
}

// TestScorecardFloor_UnknownActionOpenRejects — FIX A regression
// (fail-open on non-BUY/SELL open action). Before the fix, an
// intent=open proposal whose action normalized to anything other than
// BUY/SELL (e.g. "HOLD") derived side="" and was waved through by
// trading.EvaluateEntry as a non-open — bypassing the entire floor
// despite carrying scorecard/regime data that would otherwise reject
// it. Must now be rejected with unknown_action.
func TestScorecardFloor_UnknownActionOpenRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"HOLD","region":"us",
			 "scorecard":{"total":5},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":5}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "an open with an unrecognized action must not bypass the floor")
	assert.Contains(t, v.Detail, "unknown_action")
	assert.Contains(t, v.Detail, "AAPL")
}

// TestScorecardFloor_EmptyActionOpenRejects — FIX A regression variant:
// an intent=open proposal with an empty action string must also be
// rejected with unknown_action rather than waved through.
func TestScorecardFloor_EmptyActionOpenRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"open","action":"","region":"us",
			 "scorecard":{"total":5},
			 "regime":{"label":"RISK_ON","stale":false,"component_count":5}}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "an open with an empty action must not bypass the floor")
	assert.Contains(t, v.Detail, "unknown_action")
	assert.Contains(t, v.Detail, "AAPL")
}

// TestScorecardFloor_ProtectedSymbolLowercaseConfigRejects — FIX B
// guard. protected_symbols is operator-authored free text ("aapl")
// while the strategist's proposal carries the canonical uppercase
// ticker ("AAPL"); the protected-symbol lookup must normalize both
// sides so a lowercase config entry still matches.
func TestScorecardFloor_ProtectedSymbolLowercaseConfigRejects(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	floor := floorCfgEnabled()
	floor.ProtectedSymbols = []string{"aapl"}
	in := Input{
		TradingFloor: floor,
		ResultJSON: []byte(`{"proposals":[
			{"symbol":"AAPL","intent":"close","action":"SELL","holding_state":"held","region":"us"}
		]}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "a lowercase protected_symbols entry must still match an uppercase proposal symbol")
	assert.Contains(t, v.Detail, "protected_symbol")
	assert.Contains(t, v.Detail, "AAPL")
}

// TestScorecardFloor_HoistsFromMessageField — the envelope-merge
// fallback: proposals buried in a `message` JSON-string are still
// evaluated.
func TestScorecardFloor_HoistsFromMessageField(t *testing.T) {
	cfg := Config{Type: "scorecard_floor"}
	in := Input{
		TradingFloor: floorCfgEnabled(),
		ResultJSON: []byte(`{"status":"COMPLETED",
			"message":"{\"proposals\":[{\"symbol\":\"AAPL\",\"intent\":\"open\",\"action\":\"BUY\",\"region\":\"us\",\"scorecard\":{\"total\":1},\"regime\":{\"label\":\"RISK_ON\",\"component_count\":3}}]}"}`),
	}
	v, err := Run(context.Background(), cfg, in)
	require.NoError(t, err)
	require.NotNil(t, v, "buried below-min open must still be caught")
	assert.Contains(t, v.Detail, "below_min")
}
