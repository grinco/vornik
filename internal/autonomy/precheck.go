package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/trading"
)

// PreCheckResult is the outcome of a deterministic pre-LLM
// gate: whether to skip the tick + a human-readable reason
// for the autonomy_evaluations row. Empty Skip indicates the
// gate passed; the caller proceeds to the LLM call.
type PreCheckResult struct {
	Skip   bool
	Reason string
}

// runPreCheck dispatches on the project's configured
// autonomy.preCheck name. Empty / unknown names pass through
// (no skip), preserving back-compat for projects that haven't
// opted in.
//
// Today's catalog:
//   - "trading-rth": US Eastern market hours + broker
//     reachability + remaining-RTH ≥ workflow buffer.
//
// Adding a new pre-check is one switch case + a helper here;
// the manager call site never changes.
func (m *Manager) runPreCheck(ctx context.Context, project *registry.Project) PreCheckResult {
	switch project.Autonomy.PreCheck {
	case "":
		return PreCheckResult{}
	case "trading-rth":
		return checkTradingRTH(ctx, project)
	default:
		// Unknown pre-check name — log via the manager's
		// logger and pass through. Refusing the tick on a
		// typo would be more dangerous than silently skipping
		// the gate.
		m.logger.Warn().
			Str("project", project.ID).
			Str("preCheck", project.Autonomy.PreCheck).
			Msg("autonomy preCheck: unknown gate name; passing through")
		return PreCheckResult{}
	}
}

// checkTradingRTH refuses ticks outside US Eastern regular
// trading hours OR when remaining-RTH-time is shorter than
// the configured workflow buffer (so a tick scheduled at
// 15:55 with a 12m buffer doesn't run a workflow that would
// land orders into a closed market).
//
// Also probes the broker MCP's /caps endpoint — if unreachable,
// every place_order would fail anyway. Better to skip than to
// burn LLM budget reasoning about a tick we can't execute.
//
// Order of checks: weekday first (cheapest), then time-of-day,
// then holiday, then broker. Each gate emits its own reason
// so the operator UI distinguishes "market closed" from
// "broker offline" without parsing a generic skip message.
func checkTradingRTH(ctx context.Context, project *registry.Project) PreCheckResult {
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Fallback: refuse the tick. A misconfigured
		// timezone DB is rare but if it happens we'd
		// rather skip than guess at market hours from UTC.
		return PreCheckResult{
			Skip:   true,
			Reason: fmt.Sprintf("trading-rth: cannot load America/New_York timezone: %v", err),
		}
	}
	now := time.Now().In(tz)
	weekday := now.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return PreCheckResult{
			Skip:   true,
			Reason: fmt.Sprintf("trading-rth: market closed (weekend, %s ET)", weekday),
		}
	}
	if trading.IsUSMarketHoliday(now) {
		return PreCheckResult{
			Skip:   true,
			Reason: fmt.Sprintf("trading-rth: market closed (US holiday %s)", now.Format("2006-01-02")),
		}
	}
	// RTH window: 09:30:00 ET → 16:00:00 ET (inclusive open,
	// exclusive close). Half-day early closes (Black Friday,
	// Christmas Eve) are deliberately not modelled — the
	// half-session is live and the existing strategist gate
	// catches any 13:00 close mid-tick via the executor's
	// own current_time re-check.
	open := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, tz)
	close := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, tz)
	if now.Before(open) {
		return PreCheckResult{
			Skip:   true,
			Reason: fmt.Sprintf("trading-rth: pre-market (%s ET, opens at 09:30)", now.Format("15:04:05")),
		}
	}
	if !now.Before(close) {
		return PreCheckResult{
			Skip:   true,
			Reason: fmt.Sprintf("trading-rth: post-market (%s ET, closed at 16:00)", now.Format("15:04:05")),
		}
	}

	// Workflow-duration buffer: a tick scheduled with less
	// than this much time until close runs a workflow whose
	// executor would land into a closed market. The strategist
	// + executor each have their own market-hours fallbacks,
	// but skipping at the autonomy layer saves the entire
	// strategize + review_risk LLM cost (~$0.10/tick).
	buffer := 12 * time.Minute
	if project.Autonomy.PreCheckWorkflowMinDuration != "" {
		if d, err := time.ParseDuration(project.Autonomy.PreCheckWorkflowMinDuration); err == nil && d > 0 {
			buffer = d
		}
	}
	remaining := close.Sub(now)
	if remaining < buffer {
		return PreCheckResult{
			Skip: true,
			Reason: fmt.Sprintf(
				"trading-rth: only %s until close (need %s buffer for workflow); next tick will be next day's open",
				remaining.Round(time.Second), buffer,
			),
		}
	}

	// Broker reachability — best-effort via the public /caps
	// endpoint. Wired via VORNIK_BROKER_BASE_URL env (matches
	// what the daemon's MCP client reads) so this is config-
	// free in production. Probe failures skip the tick;
	// missing config (e.g. dispatcher-only deployments) skips
	// the broker check itself, not the tick.
	brokerURL := os.Getenv("VORNIK_BROKER_BASE_URL")
	if brokerURL == "" {
		brokerURL = "http://127.0.0.1:8788"
	}
	if !brokerReachable(ctx, brokerURL) {
		return PreCheckResult{
			Skip:   true,
			Reason: "trading-rth: broker MCP not reachable on " + brokerURL + " — skip until next tick",
		}
	}
	return PreCheckResult{}
}

// brokerReachable does a 2s GET against /caps and inspects both
// the HTTP status AND the response body for sidecar-side error
// flags. The "front door" (broker MCP HTTP server) staying up
// while the broker→IBKR-sidecar→ib_gateway pipeline behind it is
// broken is exactly how 6 trading ticks fired on 2026-05-06 against
// a sidecar that couldn't reach IBKR — every 200 came back with
// `"portfolio": null, "portfolio_error": "...connection refused"`
// in the body but the precheck was status-only and missed it.
//
// Any *_error field present (and non-empty) in /caps marks the
// broker pipeline degraded, not reachable. Conservative on
// missing-data: a parse failure on a 200 body still counts as
// reachable so we don't tank ticks on a future shape change to
// /caps that this code doesn't know about.
func brokerReachable(ctx context.Context, baseURL string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/caps", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		// 200 OK but body unreadable — be lenient (cap content
		// is best-effort, not a hard contract). Treat as
		// reachable rather than tank a tick on a network blip.
		return true
	}
	var caps map[string]any
	if err := json.Unmarshal(body, &caps); err != nil {
		// Same lenience — parse failure on the body shouldn't
		// block trading. Reachability is what we care about.
		return true
	}
	// Walk top-level fields and any *_error key with a non-empty
	// string value flips reachability to false. Catches
	// portfolio_error today, plus any future error field the
	// broker MCP grows without needing this code to be updated.
	for k, v := range caps {
		if !strings.HasSuffix(k, "_error") {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return false
		}
	}
	return true
}
