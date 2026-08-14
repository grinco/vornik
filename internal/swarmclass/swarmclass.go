// Package swarmclass classifies a swarm by the risk path it sits on.
//
// WHY A LEAF PACKAGE. The trading classification has one rule and several
// consumers that must not disagree about it: the cost/quality detector excludes
// trading swarms from scanning, the applier refuses to write to them
// (2026-07-24-applier-trading-refusal-design D3, whose whole point is that the
// two share ONE classifier and "can never diverge"), and the agent-quality
// benchmark excludes them at scan time
// (2026-08-13-agent-quality-benchmark-design §5.2).
//
// The third consumer is what moved this out of internal/service. It could not
// import that package, and copying the rule would have re-created exactly the
// divergence D3 exists to prevent — a copy that drifts is worse than no
// exclusion, because both copies still read like protection. This package holds
// no state and imports nothing, so any consumer can reach it, which is the same
// arrangement internal/promptblock uses to let registry and executor share a
// vocabulary without importing each other.
package swarmclass

import "strings"

// tradingMarkers are the substrings that put a swarm on the trading path.
//
// Substring matching, not exact names: swarm ids are operator-authored and vary
// per deployment ("ibkr-trader", "trading-research", "broker-ops"), so a fixed
// list would silently fail to protect the one swarm someone named differently.
// The cost of over-matching is a swarm excluded from benchmarking and cost
// tuning; the cost of under-matching is an automated system touching the path
// that moves real money. Those are not symmetric, and this errs deliberately.
var tradingMarkers = []string{"trader", "broker", "trading"}

// IsTrading reports whether a swarm id is on the trading path.
//
// Case-insensitive. An empty id is NOT trading — callers that must refuse an
// unknown swarm should check for empty themselves, because conflating "unknown"
// with "trading" would silently exclude every mis-wired swarm from scanning and
// present it as a safety feature.
func IsTrading(swarmID string) bool {
	s := strings.ToLower(swarmID)
	for _, marker := range tradingMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
