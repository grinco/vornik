package trading

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design: https://docs.vornik.io §Analysis-
// evidence gate (2026-09-10). Incident: twelve ibkr-trading-v2 ticks on
// 2026-09-09/10 of which six made no analysis call at all and passed the judge.

func evCfg() AnalysisEvidence {
	return AnalysisEvidence{Enabled: true, MinDailyBars: 205, BenchmarkSymbols: []string{"SPY"}}
}

func acct(positions ...string) ToolCall {
	out := `{"positions":[`
	for i, p := range positions {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return ToolCall{Name: "mcp__broker__get_account_summary", Input: "{}", Output: out + `]}`, OK: true}
}

func sc(symbol string, lastClose, sma50 float64, total, trend, momentum, macro int) ToolCall {
	return ToolCall{Name: "mcp__ta__scorecard", Input: fmt.Sprintf(`{"symbol":%q,"region":"us"}`, symbol), OK: true,
		Output: fmt.Sprintf(`{"symbol":%q,"total":%d,"trend":%d,"momentum":%d,"macro":%d,"last_close":%g,"sma50":%g,"sma20":1,"rsi14":55,"atr14":1}`,
			symbol, total, trend, momentum, macro, lastClose, sma50)}
}

func regime(region string, score int, label string) ToolCall {
	return ToolCall{Name: "mcp__ta__regime", Input: fmt.Sprintf(`{"region":%q}`, region), OK: true,
		Output: fmt.Sprintf(`{"region":%q,"score":%d,"label":%q,"stale":false,"component_count":6}`, region, score, label)}
}

func bars(symbol string, n int) ToolCall {
	out := `{"bars":[`
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += `{"close":1}`
	}
	return ToolCall{Name: "mcp__broker__get_historical_bars", Input: fmt.Sprintf(`{"symbol":%q,"duration":"1 Y"}`, symbol), Output: out + `]}`, OK: true}
}

func TestEvidence_DisabledOrNoProposalsKeyIsNoop(t *testing.T) {
	cfg := evCfg()
	cfg.Enabled = false
	assert.Nil(t, CheckAnalysisEvidence([]byte(`{"proposals":[]}`), nil, cfg, nil, nil))
	assert.Nil(t, CheckAnalysisEvidence([]byte(`{"approved":[]}`), nil, evCfg(), nil, nil))
}

func TestEvidence_RefusalReasonsInOrder(t *testing.T) {
	allowed := []string{"MSFT"}
	held := acct(`{"symbol":"JPM","qty":7}`)
	empty := []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[]}`)
	cases := []struct {
		name   string
		result []byte
		calls  []ToolCall
		reason string
	}{
		{"no snapshot", empty, []ToolCall{sc("MSFT", 1, 1, 3, 1, 1, 1)}, ReasonNoAccountSnapshot},
		{"failed snapshot does not count", empty, []ToolCall{{Name: "mcp__broker__get_account_summary", Output: `{"positions":[]}`}}, ReasonNoAccountSnapshot},
		{"no analysis at all", empty, []ToolCall{held}, ReasonNoAnalysisCalls},
		{"partial coverage", empty, []ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0)}, ReasonSymbolUnexamined},
		{"errored scorecard is not coverage", empty, []ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), {Name: "mcp__ta__scorecard", Input: `{"symbol":"JPM"}`, Output: "ValueError: stale", OK: false}}, ReasonHoldingsReviewMissing},
		{"holdings review missing", []byte(`{"proposals":[]}`), []ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 100, 90, 3, 1, 1, 1)}, ReasonHoldingsReviewMissing},
		{"holdings review value mismatch", []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","last_close":101,"sma50":90,"verdict":"hold"}]}`),
			[]ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 100, 90, 3, 1, 1, 1)}, ReasonHoldingsReviewMismatch},
		{"exit rule: below sma50 but hold", []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","last_close":80,"sma50":90,"verdict":"hold"}]}`),
			[]ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 80, 90, -3, -1, -1, -1)}, ReasonExitRuleViolated},
		{"exit rule: close verdict without close proposal", []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","last_close":80,"sma50":90,"verdict":"close"}]}`),
			[]ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 80, 90, -3, -1, -1, -1)}, ReasonExitRuleViolated},
		{"unavailable claimed while scorecard succeeded", []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","verdict":"unavailable"}]}`),
			[]ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 100, 90, 3, 1, 1, 1), bars("JPM", 60)}, ReasonHoldingsReviewMismatch},
		{"open proposal unscored", []byte(`{"proposals":[{"symbol":"MSFT","intent":"open","action":"BUY","region":"us","scorecard":{"total":3,"trend":1,"momentum":1,"macro":1},"regime":{"score":1,"label":"RISK_ON"}}],"holdings_review":[]}`),
			[]ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), bars("MSFT", 300), regime("us", 1, "RISK_ON")}, ReasonProposalUnscored},
		{"open proposal carries numbers the tool did not return", []byte(`{"proposals":[{"symbol":"MSFT","intent":"open","action":"BUY","region":"us","scorecard":{"total":4,"trend":1,"momentum":1,"macro":1},"regime":{"score":1,"label":"RISK_ON"}}],"holdings_review":[]}`),
			[]ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 3, 1, 1, 1), regime("us", 1, "RISK_ON")}, ReasonProposalMismatch},
		{"open proposal region unscored", []byte(`{"proposals":[{"symbol":"MSFT","intent":"open","action":"BUY","region":"eu","scorecard":{"total":3,"trend":1,"momentum":1,"macro":1},"regime":{"score":1,"label":"RISK_ON"}}],"holdings_review":[]}`),
			[]ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 3, 1, 1, 1), regime("us", 1, "RISK_ON")}, ReasonRegionUnscored},
		{"open proposal regime mismatch", []byte(`{"proposals":[{"symbol":"MSFT","intent":"open","action":"BUY","region":"us","scorecard":{"total":3,"trend":1,"momentum":1,"macro":1},"regime":{"score":2,"label":"RISK_ON"}}],"holdings_review":[]}`),
			[]ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 3, 1, 1, 1), regime("us", 1, "RISK_ON")}, ReasonProposalMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckAnalysisEvidence(tc.result, tc.calls, evCfg(), allowed, nil)
			require.NotNil(t, r, "expected refusal")
			assert.Equal(t, tc.reason, r.Reason, r.Detail)
			assert.NotEmpty(t, r.Detail)
		})
	}
	// Omitting a held symbol whose scorecard errored is a refusal that names
	// the symbol, not a way around the unavailable-verdict rule.
	r := CheckAnalysisEvidence(empty, []ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0),
		{Name: "mcp__ta__scorecard", Input: `{"symbol":"JPM"}`, Output: "ValueError: stale", OK: false}}, evCfg(), allowed, nil)
	require.NotNil(t, r)
	assert.Equal(t, ReasonHoldingsReviewMissing, r.Reason)
	assert.Contains(t, r.Detail, "JPM")
	// A wrapped or non-JSON account output is not a snapshot; the gate fails
	// closed rather than treating an unparsable book as flat.
	wrapped := ToolCall{Name: "mcp__broker__get_account_summary", Output: `{"result":{"positions":[]}}`, OK: true}
	r = CheckAnalysisEvidence(empty, []ToolCall{wrapped, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0)}, evCfg(), allowed, nil)
	require.NotNil(t, r)
	assert.Equal(t, ReasonNoAccountSnapshot, r.Reason)
	// Values that would round differently at the fourth decimal refuse.
	r = CheckAnalysisEvidence([]byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","last_close":100.1235,"sma50":90.5,"verdict":"hold"}]}`),
		[]ToolCall{held, sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 0, 0, 0, 0), sc("JPM", 100.1234, 90.5, 3, 1, 1, 1)}, evCfg(), allowed, nil)
	require.NotNil(t, r)
	assert.Equal(t, ReasonHoldingsReviewMismatch, r.Reason)
}

func TestEvidence_Passes(t *testing.T) {
	allowed := []string{"MSFT"}
	t.Run("hold with matching review", func(t *testing.T) {
		calls := []ToolCall{acct(`{"symbol":"JPM","qty":7}`), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 2, 1, 1, 0), sc("JPM", 100.1234, 90.5, 3, 1, 1, 1)}
		result := []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[{"symbol":"jpm","last_close":100.1234,"sma50":90.5,"verdict":"hold"}]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, nil))
	})
	t.Run("close proposal satisfies exit rule for a short too", func(t *testing.T) {
		calls := []ToolCall{acct(`{"symbol":"JPM","qty":-7}`), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 2, 1, 1, 0), sc("JPM", 80, 90, -3, -1, -1, -1)}
		result := []byte(`{"proposals":[{"symbol":"JPM","intent":"close","action":"BUY","qty":7}],"has_proposals":true,"holdings_review":[{"symbol":"JPM","last_close":80,"sma50":90,"verdict":"close"}]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, nil))
	})
	t.Run("protected symbol below sma50 holds", func(t *testing.T) {
		calls := []ToolCall{acct(`{"symbol":"JPM","qty":7}`), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 2, 1, 1, 0), sc("JPM", 80, 90, -3, -1, -1, -1)}
		result := []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","last_close":80,"sma50":90,"verdict":"hold"}]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, []string{"JPM"}))
	})
	t.Run("unavailable backed by broker bars", func(t *testing.T) {
		calls := []ToolCall{acct(`{"symbol":"JPM","qty":7}`), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 2, 1, 1, 0),
			{Name: "mcp__ta__scorecard", Input: `{"symbol":"JPM"}`, Output: "ValueError: stale", OK: false}, bars("JPM", 60)}
		result := []byte(`{"proposals":[],"holdings_review":[{"symbol":"JPM","verdict":"unavailable"}]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, nil))
	})
	t.Run("bars fallback covers a universe symbol", func(t *testing.T) {
		calls := []ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), bars("MSFT", 205)}
		assert.Nil(t, CheckAnalysisEvidence([]byte(`{"proposals":[],"holdings_review":[]}`), calls, evCfg(), allowed, nil))
		short := []ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), bars("MSFT", 204)}
		r := CheckAnalysisEvidence([]byte(`{"proposals":[],"holdings_review":[]}`), short, evCfg(), allowed, nil)
		require.NotNil(t, r)
		assert.Equal(t, ReasonSymbolUnexamined, r.Reason)
	})
	t.Run("valid open carries tool values verbatim", func(t *testing.T) {
		calls := []ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 3, 1, 1, 1), regime("us", 1, "RISK_ON")}
		result := []byte(`{"proposals":[{"symbol":"MSFT","action":"BUY","region":"us","scorecard":{"total":3,"trend":1,"momentum":1,"macro":1},"regime":{"score":1,"label":"RISK_ON","stale":false,"component_count":6}}],"holdings_review":[]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, nil))
	})
	t.Run("eu proposal checks the eu regime call", func(t *testing.T) {
		calls := []ToolCall{acct(), sc("SPY", 1, 1, 0, 0, 0, 0), sc("MSFT", 1, 1, 3, 1, 1, 1), regime("us", -1, "RISK_OFF"), regime("eu", 2, "RISK_ON")}
		result := []byte(`{"proposals":[{"symbol":"MSFT","action":"BUY","region":"EU","scorecard":{"total":3,"trend":1,"momentum":1,"macro":1},"regime":{"score":2,"label":"risk_on"}}],"holdings_review":[]}`)
		assert.Nil(t, CheckAnalysisEvidence(result, calls, evCfg(), allowed, nil))
	})
}

func TestAnalysisEvidenceValidate(t *testing.T) {
	require.NoError(t, AnalysisEvidence{}.Validate())
	require.NoError(t, evCfg().Validate())
	require.Error(t, AnalysisEvidence{Enabled: true}.Validate(), "enabled needs a positive bar minimum")
	require.Error(t, AnalysisEvidence{MinDailyBars: -1}.Validate())
}
