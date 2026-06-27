package dispatcher

import (
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
)

// outputGuardConfig holds the dispatcher's per-Agent guard
// configuration. Distinct from outputguard.Report because the
// dispatcher has knobs the library doesn't (RedactHigh — the
// library always produces a redact-able report, but the
// dispatcher chooses whether to apply Redact() before handing
// the body to the LLM).
type outputGuardConfig struct {
	// RedactHigh controls whether HIGH-severity findings get
	// redacted in place before the LLM sees the result. Default
	// true. When false, the dispatcher still records the finding
	// but the LLM sees the raw content (useful for offline
	// adversarial-testing setups).
	RedactHigh bool
}

// GuardWarning rides back on Result so the UI / Telegram bot can
// render a non-jargon banner alongside the assistant reply.
// Deliberately summary-only — the per-finding detail (byte spans,
// evidence) lives in the tool_audit_log row, not in the operator
// banner.
type GuardWarning struct {
	// Tool is the function name the finding was attached to.
	Tool string
	// MaxSeverity is the worst tier observed across the tool's
	// findings. Drives the banner colour: info → blue,
	// warn → amber, high → red (UI's call).
	MaxSeverity outputguard.Severity
	// Kinds is the de-duplicated list of finding-kind labels
	// (injection_instruction, credential_pattern, encoded_blob,
	// etc.) so the operator sees "what the guard flagged"
	// without a click-through.
	Kinds []string
	// Redacted is true when HIGH findings were redacted in place.
	// Lets the UI render a slightly different message ("auto-
	// redacted in context") vs. the un-redacted INFO/WARN case.
	Redacted bool
}

// applyOutputGuard runs the guard on a single tool result and
// produces (scanned-or-redacted body, optional warning). Empty
// findings return the body unchanged and a zero-value
// GuardWarning (caller checks `MaxSeverity != ""`).
//
// prov carries the content's trust level. First-party content
// (dispatcher builtins, task_output artifacts) has injection-class
// rules skipped; secret-class rules always run. Unknown/third-party
// provenance runs the full rule set (fail-safe default).
//
// Failure modes are silent — a malformed pattern in the library
// that panicked would otherwise take down the dispatcher loop.
// We recover and return the original body so the worst case is
// "guard didn't fire this turn", not "request 500s".
//
// metrics (may be nil) receives one findings_total observation per
// finding (labelled tool/severity/kind), a redactions_total bump when
// HIGH content is rewritten, and the per-call scan_duration_seconds.
// The scan is always timed — even a no-finding scan exercises the
// regex set, and operators want the latency floor — so the duration
// observation rides outside the HasFinding short-circuit.
func (c *outputGuardConfig) applyOutputGuard(toolName, body string, prov outputguard.Provenance, metrics *Metrics) (string, GuardWarning) {
	if c == nil {
		return body, GuardWarning{}
	}
	defer func() { _ = recover() }()

	start := time.Now()
	rep := outputguard.ScanWithProvenance(body, prov)
	metrics.observeOutputGuardScan(toolName, time.Since(start))
	if !rep.HasFinding() {
		return body, GuardWarning{}
	}
	metrics.observeOutputGuardFindings(toolName, rep)
	w := GuardWarning{
		Tool:        toolName,
		MaxSeverity: rep.MaxSeverity(),
		Kinds:       kindsSummary(rep),
	}
	if w.MaxSeverity == outputguard.SeverityHigh && c.RedactHigh {
		body = outputguard.Redact(body, rep)
		w.Redacted = true
		metrics.observeOutputGuardRedaction(toolName)
	}
	return body, w
}

// kindsSummary collects unique Kind labels from a Report and
// returns them sorted. The sort is for stable rendering in the
// UI — the underlying library doesn't guarantee finding order.
func kindsSummary(rep outputguard.Report) []string {
	if len(rep.Findings) == 0 {
		return nil
	}
	seen := make(map[outputguard.Kind]struct{}, len(rep.Findings))
	out := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		if _, ok := seen[f.Kind]; ok {
			continue
		}
		seen[f.Kind] = struct{}{}
		out = append(out, string(f.Kind))
	}
	sort.Strings(out)
	return out
}

// summarizeGuard is a small helper used by the dispatcher's log
// emission so we have a single line per tool call regardless of
// how many findings the report contained.
func summarizeGuard(w GuardWarning) string {
	if w.MaxSeverity == "" {
		return ""
	}
	return string(w.MaxSeverity) + ":" + strings.Join(w.Kinds, ",")
}
