package featuredoctor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/version"
)

// tradingFeature validates the trading equity time-series for every
// trading-enabled project. Unlike other features it has no config gate —
// trading is enabled per-project via a `broker` MCP server, not a global key —
// so gatesOn is always true and the Verify check (which summarizes per-project
// series findings) always runs. Surface-only: the result shows on the doctor
// page; the adapter also emits a metric + log. See
// https://docs.vornik.io
func tradingFeature() Feature {
	return Feature{
		ID:      "trading-series",
		Title:   "Trading time-series integrity",
		Summary: "Validates the equity-sampler snapshot series per trading-enabled project: sample-cadence gaps, duplicate timestamps, staleness, and implausible values.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/trading-series.md",
		Edition: version.EditionEnterprise,
		Verify:  verifyTradingSeries,
		Apply:   ReloadHot,
	}
}

func verifyTradingSeries(ctx context.Context, d Deps) PrereqResult {
	if d.Trading == nil {
		// Graceful skip in minimal deployments (no trading repos wired).
		return PrereqResult{OK: true, Detail: "trading-series probe not wired (no trading data sources)"}
	}
	findings, err := d.Trading.ValidateSeries(ctx)
	if err != nil {
		return PrereqResult{
			OK:          false,
			Detail:      "series validation failed: " + err.Error(),
			Remediation: "check the trading snapshot repository / DB connectivity",
		}
	}
	if len(findings) == 0 {
		return PrereqResult{OK: true, Detail: "trading series healthy (no anomalies)"}
	}
	return PrereqResult{
		OK:          false,
		Detail:      summarizeFindings(findings),
		Remediation: "inspect the equity sampler (5-min cadence, leader-gated) and the broker connection; a stale or gappy series usually means the sampler or IB Gateway was down. out_of_bounds / non_monotonic indicate a data-integrity bug worth a closer look.",
	}
}

// summarizeFindings renders a stable, readable one-liner: per-code counts plus
// the first few concrete examples.
func summarizeFindings(findings []TradingSeriesFinding) string {
	byCode := map[string]int{}
	for _, f := range findings {
		byCode[f.Code]++
	}
	codes := make([]string, 0, len(byCode))
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%s×%d", c, byCode[c]))
	}

	const maxExamples = 3
	examples := make([]string, 0, maxExamples)
	for _, f := range findings {
		if len(examples) >= maxExamples {
			break
		}
		examples = append(examples, fmt.Sprintf("%s/%s: %s", f.ProjectID, f.Code, f.Detail))
	}
	more := ""
	if len(findings) > maxExamples {
		more = fmt.Sprintf(" (+%d more)", len(findings)-maxExamples)
	}
	return fmt.Sprintf("%d anomaly(ies) [%s] — %s%s",
		len(findings), strings.Join(parts, ", "), strings.Join(examples, "; "), more)
}
