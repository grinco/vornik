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
	if isUSMarketHoliday(now) {
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

// isUSMarketHoliday reports whether the given local date (in
// ET) is a full-close US equity market holiday. Mirrors the
// list the strategist uses in its operating-window prompt so
// the daemon-side gate and the agent-side fallback agree.
//
// The half-day closes (day after Thanksgiving, Christmas Eve)
// are deliberately omitted — markets are open 09:30-13:00, so
// the standard RTH check would refuse half the live session.
// The strategist's mid-execution check at 13:00 catches an
// edge tick scheduled too late.
//
// Easter-relative dates (Good Friday) are computed via the
// Anonymous Gregorian algorithm so the function stays
// table-free across years.
func isUSMarketHoliday(t time.Time) bool {
	year, month, day := t.Date()
	// New Year's Day (observed: weekend pushes to Monday).
	if month == time.January {
		switch t.Weekday() {
		case time.Monday:
			if day == 1 || day == 2 || day == 3 {
				// Jan 1 Monday = direct; Jan 2 Monday = Sunday-pushed; Jan 3 = Saturday from prior year? Saturday holidays push BACK to Friday for NYSE, not Monday.
				if day == 1 {
					return true
				}
				if day == 2 && time.Date(year, 1, 1, 0, 0, 0, 0, t.Location()).Weekday() == time.Sunday {
					return true
				}
			}
		default:
			if day == 1 && t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
				return true
			}
		}
	}
	// MLK Day — third Monday of January.
	if month == time.January && t.Weekday() == time.Monday && day >= 15 && day <= 21 {
		return true
	}
	// Presidents Day — third Monday of February.
	if month == time.February && t.Weekday() == time.Monday && day >= 15 && day <= 21 {
		return true
	}
	// Good Friday — compare on Y/M/D, not the full time, so a
	// mid-day check still matches against the algorithm's
	// midnight return value.
	gfYear, gfMonth, gfDay := easterFriday(year, t.Location()).Date()
	if year == gfYear && month == gfMonth && day == gfDay {
		return true
	}
	// Memorial Day — last Monday of May.
	if month == time.May && t.Weekday() == time.Monday && day >= 25 {
		return true
	}
	// Juneteenth — Jun 19, observed.
	if month == time.June && day == 19 && t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
		return true
	}
	if month == time.June && day == 20 && t.Weekday() == time.Monday {
		// Jun 19 was Sunday → observed Mon Jun 20.
		return true
	}
	if month == time.June && day == 18 && t.Weekday() == time.Friday {
		// Jun 19 was Saturday → observed Fri Jun 18.
		return true
	}
	// Independence Day — Jul 4, observed (same weekend rule).
	if month == time.July && day == 4 && t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
		return true
	}
	if month == time.July && day == 5 && t.Weekday() == time.Monday {
		return true
	}
	if month == time.July && day == 3 && t.Weekday() == time.Friday {
		return true
	}
	// Labor Day — first Monday of September.
	if month == time.September && t.Weekday() == time.Monday && day <= 7 {
		return true
	}
	// Thanksgiving — fourth Thursday of November.
	if month == time.November && t.Weekday() == time.Thursday && day >= 22 && day <= 28 {
		return true
	}
	// Christmas — Dec 25, observed.
	if month == time.December && day == 25 && t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
		return true
	}
	if month == time.December && day == 26 && t.Weekday() == time.Monday {
		return true
	}
	if month == time.December && day == 24 && t.Weekday() == time.Friday {
		return true
	}
	return false
}

// easterFriday returns the Good Friday date for the given
// year, in the supplied location at midnight. Anonymous
// Gregorian algorithm — accurate for the Gregorian calendar
// (1583+).
func easterFriday(year int, loc *time.Location) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	easter := time.Date(year, time.Month(month), day, 0, 0, 0, 0, loc)
	return easter.AddDate(0, 0, -2) // Good Friday is 2 days before Easter Sunday.
}
