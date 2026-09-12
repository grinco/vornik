package verifier

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"vornik.io/vornik/internal/trading"
)

type policyProposal struct {
	Symbol        string  `json:"symbol"`
	Intent        string  `json:"intent"`
	Action        string  `json:"action"`
	OrderType     string  `json:"order_type"`
	Qty           float64 `json:"qty"`
	LimitPrice    float64 `json:"limit_price"`
	StopLossPrice float64 `json:"stop_loss_price"`
}

// FilterTradingEntryPolicy gates both strategist proposals and risk approvals.
// It preserves closes and retained JSON verbatim, records dropped candidates,
// and updates routing booleans. This is a workflow gate; execution must still
// recheck live positions and prices against the broker's safety envelope.
func FilterTradingEntryPolicy(raw []byte, policy trading.EntryPolicy) ([]byte, error) {
	if !policy.Enabled {
		return raw, nil
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := jsonUnmarshalLenient(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("entry policy: invalid result object")
	}
	changed := false
	for _, key := range []string{"proposals", "approved"} {
		if _, ok := obj[key]; !ok {
			continue
		}
		if err := filterEntryArray(obj, key, policy); err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(obj)
}

func filterEntryArray(obj map[string]json.RawMessage, key string, policy trading.EntryPolicy) error {
	var proposals []json.RawMessage
	if err := json.Unmarshal(obj[key], &proposals); err != nil {
		return fmt.Errorf("entry policy: invalid %s array: %w", key, err)
	}
	kept := make([]json.RawMessage, 0, len(proposals))
	rejected := make([]map[string]string, 0)
	entries := 0
	seen := make(map[string]bool)
	for _, raw := range proposals {
		var p policyProposal
		if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.Symbol) == "" {
			return fmt.Errorf("entry policy: invalid proposal")
		}
		p.Intent = strings.ToLower(strings.TrimSpace(p.Intent))
		if p.Intent != "" && p.Intent != "open" && p.Intent != "close" {
			return fmt.Errorf("entry policy: unknown intent %q", p.Intent)
		}
		if p.Intent == "close" {
			kept = append(kept, raw)
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
		reason := entryPolicyReason(p, policy)
		if reason == "" && (entries >= policy.MaxEntriesPerTick || seen[sym]) {
			reason = "entry_count_or_duplicate"
		}
		if reason != "" {
			rejected = append(rejected, map[string]string{"symbol": p.Symbol, "reason": reason, "stage": key})
			incFloorRejected(reason)
			continue
		}
		entries++
		seen[sym] = true
		kept = append(kept, raw)
	}
	// RawMessages were parsed above; marshal still propagates errors fail-closed.
	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	obj[key] = encoded
	flag := "has_proposals"
	if key == "approved" {
		flag = "has_approvals"
	}
	obj[flag] = json.RawMessage(fmt.Sprintf("%t", len(kept) > 0))
	if len(rejected) > 0 {
		obj["entry_policy_rejections"], err = json.Marshal(rejected)
	}
	return err
}

func entryPolicyReason(p policyProposal, policy trading.EntryPolicy) string {
	allowed := false
	for _, sym := range policy.AllowedSymbols {
		if strings.EqualFold(strings.TrimSpace(sym), strings.TrimSpace(p.Symbol)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "entry_symbol_not_allowed"
	}
	action := strings.ToUpper(strings.TrimSpace(p.Action))
	if (action != "BUY" && action != "SELL") || (policy.LongOnly && action != "BUY") {
		return "entry_direction_not_allowed"
	}
	if strings.ToUpper(strings.TrimSpace(p.OrderType)) != "LMT" {
		return "entry_requires_limit"
	}
	if p.Qty <= 0 || p.LimitPrice <= 0 || p.StopLossPrice <= 0 ||
		(action == "BUY" && p.StopLossPrice >= p.LimitPrice) ||
		(action == "SELL" && p.StopLossPrice <= p.LimitPrice) {
		return "entry_invalid_size_or_stop"
	}
	notional := p.Qty * p.LimitPrice
	risk := p.Qty * math.Abs(p.LimitPrice-p.StopLossPrice)
	if math.IsInf(notional, 0) || math.IsInf(risk, 0) || risk > policy.MaxRiskUSD {
		return "entry_risk_exceeded"
	}
	if notional < policy.MinNotionalUSD {
		return "entry_below_min_notional"
	}
	return ""
}
