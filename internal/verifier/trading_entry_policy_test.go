package verifier

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/trading"
)

func TestTradingEntryPolicy(t *testing.T) {
	policy := trading.EntryPolicy{Enabled: true, AllowedSymbols: []string{"MSFT"}, LongOnly: true, MaxRiskUSD: 50, MinNotionalUSD: 500, MaxEntriesPerTick: 1}
	valid := `{"symbol":"MSFT","intent":"open","action":"BUY","qty":2,"limit_price":500,"stop_loss_price":480,"order_type":"LMT"}`
	for _, field := range []string{"proposals", "approved"} {
		t.Run(field, func(t *testing.T) {
			input := []byte(`{"` + field + `":[{"symbol":"INFY","intent":"open","action":"BUY","qty":50,"limit_price":12,"stop_loss_price":11},` + valid + `,` + valid + `,{"symbol":"NVDA","intent":"close","action":"SELL","qty":6}],"note":"preserve"}`)
			out, err := FilterTradingEntryPolicy(input, policy)
			require.NoError(t, err)
			var decoded map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &decoded))
			require.NotContains(t, string(decoded[field]), "INFY")
			var kept []policyProposal
			require.NoError(t, json.Unmarshal(decoded[field], &kept))
			require.Len(t, kept, 2)
			require.Equal(t, "NVDA", kept[1].Symbol)
			require.Contains(t, string(out), "preserve")
			flag := "has_proposals"
			if field == "approved" {
				flag = "has_approvals"
			}
			require.Contains(t, string(out), `"`+flag+`":true`)
			require.Contains(t, string(out), "entry_policy_rejections")
		})
	}
	for name, proposal := range map[string]string{
		"short":      `{"symbol":"MSFT","action":"SELL","qty":2,"limit_price":500,"stop_loss_price":520,"order_type":"LMT"}`,
		"risk":       `{"symbol":"MSFT","action":"BUY","qty":4,"limit_price":500,"stop_loss_price":480,"order_type":"LMT"}`,
		"small":      `{"symbol":"MSFT","action":"BUY","qty":0.2,"limit_price":500,"stop_loss_price":480,"order_type":"LMT"}`,
		"wrong_stop": `{"symbol":"MSFT","action":"BUY","qty":2,"limit_price":500,"stop_loss_price":520,"order_type":"LMT"}`,
		"market":     `{"symbol":"MSFT","action":"BUY","qty":2,"limit_price":500,"stop_loss_price":480,"order_type":"MKT"}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := FilterTradingEntryPolicy([]byte(`{"proposals":[`+proposal+`]}`), policy)
			require.NoError(t, err)
			require.Contains(t, string(out), `"has_proposals":false`)
			require.Contains(t, string(out), `"proposals":[]`)
		})
	}
	for _, input := range []string{`broken`, `{"proposals":{}}`, `{"proposals":[null]}`, `{"proposals":[{"intent":"typo"}]}`} {
		_, err := FilterTradingEntryPolicy([]byte(input), policy)
		require.Error(t, err, input)
	}
	input := []byte(`{"placed":[],"skipped":[]}`)
	out, err := FilterTradingEntryPolicy(input, policy)
	require.NoError(t, err)
	require.Equal(t, input, out)
	policy.Enabled = false
	out, err = FilterTradingEntryPolicy([]byte(`broken`), policy)
	require.NoError(t, err)
	require.Equal(t, "broken", string(out))
}
