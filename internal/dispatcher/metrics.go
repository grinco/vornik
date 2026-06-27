package dispatcher

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/outputguard"
)

// Metrics holds Prometheus metrics for the dispatcher.
type Metrics struct {
	ToolCallsTotal *prometheus.CounterVec

	// ContextTierTotal counts per-turn tier emissions, labelled by
	// project + tier name. Operators alert on a sustained rate of
	// "degrading" / "poor" in a project — a leading indicator that
	// the chat surface is approaching context exhaustion and the
	// deferred-tool path has engaged.
	ContextTierTotal *prometheus.CounterVec

	// ContextHeadroomPct is a histogram of remaining-budget % per
	// turn (100 - used/limit). Used to tune the PEAK/GOOD/DEGRADING/
	// POOR thresholds: if the bulk of turns sit in 30-50% headroom,
	// the GOOD boundary may be too generous. Bucket layout favours
	// the low-headroom tail (where degradation matters) without
	// burning labels on the dense PEAK band.
	ContextHeadroomPct *prometheus.HistogramVec

	// OutputGuardFindingsTotal counts every output-guard finding on a
	// tool result, labelled by tool, severity (info/warn/high) and the
	// finding kind (injection_instruction, credential_pattern, …). One
	// Inc per finding — a single scan can produce several. The
	// dispatcher output guard is a compliance surface (it redacts
	// leaked credentials before the LLM sees them); before this metric
	// landed operators had no rate signal at all (audit N3, §3.3).
	OutputGuardFindingsTotal *prometheus.CounterVec

	// OutputGuardRedactionsTotal counts scans where HIGH-severity
	// content was rewritten in place, labelled by tool. A redaction is
	// the one guard action that mutates what the LLM sees, so it gets
	// its own series rather than being inferred from the findings
	// counter.
	OutputGuardRedactionsTotal *prometheus.CounterVec

	// OutputGuardScanDuration is the per-call wall-clock of one guard
	// scan, labelled by tool. Observed on every scan including
	// no-finding scans so the histogram captures the regex-set latency
	// floor.
	OutputGuardScanDuration *prometheus.HistogramVec
}

// NewMetrics creates dispatcher metrics registered against the given registerer.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	auto := promauto.With(registerer)
	return &Metrics{
		ToolCallsTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "dispatcher",
				Name:      "tool_calls_total",
				Help:      "Total tool calls executed by the dispatcher.",
			},
			[]string{"tool"},
		),
		ContextTierTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "chat",
				Name:      "context_tier_total",
				Help:      "Per-turn chat context-budget tier emissions (PEAK / GOOD / DEGRADING / POOR).",
			},
			[]string{"project", "tier"},
		),
		ContextHeadroomPct: auto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "vornik",
				Subsystem: "chat",
				Name:      "context_headroom_pct",
				Help:      "Per-turn remaining context-budget percentage (0 = exhausted, 100 = empty conversation).",
				// Tail-weighted: a 5% step in the low-headroom band
				// matters far more than the same step at the top.
				Buckets: []float64{0, 5, 10, 15, 25, 40, 60, 70, 85, 100},
			},
			[]string{"project"},
		),
		OutputGuardFindingsTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "dispatcher",
				Name:      "output_guard_findings_total",
				Help:      "Output-guard findings on tool results, labelled by tool, severity and finding kind.",
			},
			[]string{"tool", "severity", "kind"},
		),
		OutputGuardRedactionsTotal: auto.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "dispatcher",
				Name:      "output_guard_redactions_total",
				Help:      "Output-guard scans that redacted HIGH-severity content in place, labelled by tool.",
			},
			[]string{"tool"},
		),
		OutputGuardScanDuration: auto.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "vornik",
				Subsystem: "dispatcher",
				Name:      "output_guard_scan_duration_seconds",
				Help:      "Wall-clock duration of one output-guard scan, labelled by tool.",
				// Regex-set scan over a tool result: microseconds to a
				// few ms for large bodies. Buckets centred there.
				Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05},
			},
			[]string{"tool"},
		),
	}
}

// observeOutputGuardScan records the per-call scan duration. Nil-safe
// — the dispatcher may run without wired metrics (tests, SQLite).
func (m *Metrics) observeOutputGuardScan(tool string, d time.Duration) {
	if m == nil || m.OutputGuardScanDuration == nil {
		return
	}
	m.OutputGuardScanDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// observeOutputGuardFindings bumps OutputGuardFindingsTotal once per
// finding in the report, labelled by tool, severity and kind. Nil-safe.
func (m *Metrics) observeOutputGuardFindings(tool string, rep outputguard.Report) {
	if m == nil || m.OutputGuardFindingsTotal == nil {
		return
	}
	for _, f := range rep.Findings {
		m.OutputGuardFindingsTotal.WithLabelValues(tool, string(f.Severity), string(f.Kind)).Inc()
	}
}

// observeOutputGuardRedaction bumps OutputGuardRedactionsTotal for a
// scan that rewrote HIGH content in place. Nil-safe.
func (m *Metrics) observeOutputGuardRedaction(tool string) {
	if m == nil || m.OutputGuardRedactionsTotal == nil {
		return
	}
	m.OutputGuardRedactionsTotal.WithLabelValues(tool).Inc()
}

func (m *Metrics) recordToolCall(tool string) {
	if m == nil {
		return
	}
	m.ToolCallsTotal.WithLabelValues(tool).Inc()
}

// recordContextTier bumps the tier counter + headroom histogram for
// one dispatcher turn. project may be empty when the dispatcher runs
// without a pinned active project (sub-agent paths); the metric still
// records but with an empty label so the alert query can split "no
// project" from real projects. headroomPct is the [0, 100] value
// produced by chat.HeadroomPct.
//
// Nil-receiver safe — the dispatcher Process loop calls this on every
// turn including when Metrics was never wired (tests, deployments
// with telemetry disabled).
func (m *Metrics) recordContextTier(project string, tier chat.ContextTier, headroomPct float64) {
	if m == nil {
		return
	}
	if m.ContextTierTotal != nil {
		m.ContextTierTotal.WithLabelValues(project, tier.String()).Inc()
	}
	if m.ContextHeadroomPct != nil {
		m.ContextHeadroomPct.WithLabelValues(project).Observe(headroomPct)
	}
}

// ObserveContextTier is the exported counterpart of recordContextTier
// — same semantics, exposed so the api package (chat-proxy) can share
// the same timeseries with the dispatcher's Telegram / UI turns
// without re-declaring the metric.
func (m *Metrics) ObserveContextTier(project string, tier chat.ContextTier, headroomPct float64) {
	m.recordContextTier(project, tier, headroomPct)
}
