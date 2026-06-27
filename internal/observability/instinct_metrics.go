package observability

// Re-homed from internal/instinct/metrics.go (Phase 1c).
// The instinct metric type is pure Prometheus infrastructure — no IP, no
// learning heuristics. Moving it here lets CE packages (executor, memetic)
// import the metrics type without importing internal/instinct (IP).
//
// The emit API is identical to the original internal/instinct.Metrics so
// call sites change only their import path (Tasks 2 and 3).
//
// Design reference: https://docs.vornik.io §2.1
// ("What STAYS CE: Instinct metrics re-home to internal/observability")

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	instNamespace = "vornik"
	instSubsystem = "instinct"
)

// InstinctMetrics holds the Prometheus metrics for the instinct extraction
// worker (slice 1) plus the vornik_instinct_total population gauge, which
// the worker refreshes each tick from CountByDomainStatus.
// vornik_instinct_applications_total (ApplicationsTotal) is registered
// unconditionally by NewInstinctMetrics alongside the rest; it simply stays
// at zero until a consumer writes instinct_applications rows.
//
// All fields are nil-safe at the call sites — a worker built without
// InstinctMetrics simply doesn't emit.
type InstinctMetrics struct {
	// ExtractionTicksTotal counts worker ticks by outcome:
	// idle (nothing to do) | progressed (≥1 instinct touched) |
	// errored (the tick failed before completing).
	ExtractionTicksTotal *prometheus.CounterVec

	// ExtractionDurationSeconds is the end-to-end per-tick latency
	// (query + extract + upsert), so operators see the real cost of
	// leaving the worker on.
	ExtractionDurationSeconds prometheus.Histogram

	// InstinctsUpsertedTotal counts instinct upserts by domain — the
	// throughput of learning.
	InstinctsUpsertedTotal *prometheus.CounterVec

	// EvidenceAddedTotal counts newly-inserted evidence rows by polarity.
	// Re-seen outcomes (idempotent no-ops) are NOT counted, so this is the
	// rate of genuinely new corroboration.
	EvidenceAddedTotal *prometheus.CounterVec

	// InstinctTotal is the live instinct population by (domain, status).
	// A gauge, refreshed each worker tick from CountByDomainStatus —
	// Reset() first so buckets that drop to zero disappear instead of
	// sticking at their last value.
	InstinctTotal *prometheus.GaugeVec

	// DistillationsTotal counts the advisory instincts produced by the
	// cheap-model distillation pass. A plain counter — cache short-circuit
	// and decline-to-generalise cases simply don't bump it, so this tracks
	// genuine LLM-derived candidates only.
	DistillationsTotal prometheus.Counter

	// ApplicationsTotal counts instinct applications (surfacings + their
	// feedback) by surface and result. surface ∈ {failed_task_ui,
	// lead_recovery, architect_evidence}; result ∈ {accepted, rejected,
	// succeeded, failed, ignored}. Nil-safe at every call site.
	ApplicationsTotal *prometheus.CounterVec

	// GlobalConflictsTotal counts cross-project promotion conflicts (W6):
	// two projects promoting CONTRADICTORY actions for one trigger_key.
	// resolution ∈ {replaced, kept_incumbent}. Stays at zero until
	// promote_projects ≥ 2 actually mints cross-project globals.
	GlobalConflictsTotal *prometheus.CounterVec
}

// NewInstinctMetrics registers and returns the instinct worker metrics. A nil
// registerer falls back to the default Prometheus registerer.
//
// The function name matches the original instinct.NewMetrics; callers need
// only change the import path (from internal/instinct to internal/observability)
// and the call to observability.NewInstinctMetrics.
func NewInstinctMetrics(registerer prometheus.Registerer) *InstinctMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	pa := promauto.With(registerer)
	return &InstinctMetrics{
		ExtractionTicksTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "extraction_ticks_total",
			Help:      "Instinct extraction worker ticks by outcome (idle|progressed|errored).",
		}, []string{"outcome"}),
		ExtractionDurationSeconds: pa.NewHistogram(prometheus.HistogramOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "extraction_duration_seconds",
			Help:      "End-to-end per-tick instinct extraction latency.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		InstinctsUpsertedTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "upserted_total",
			Help:      "Instinct upserts by domain.",
		}, []string{"domain"}),
		EvidenceAddedTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "evidence_added_total",
			Help:      "Newly-inserted instinct evidence rows by polarity (idempotent re-sees excluded).",
		}, []string{"polarity"}),
		InstinctTotal: pa.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "total",
			Help:      "Live instinct population by domain and status.",
		}, []string{"domain", "status"}),
		DistillationsTotal: pa.NewCounter(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "distillations_total",
			Help:      "Advisory instincts produced by the cheap-model distillation pass (cache hits and declines excluded).",
		}),
		ApplicationsTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "applications_total",
			Help:      "Instinct applications by surface (failed_task_ui|lead_recovery|architect_evidence) and result (accepted|rejected|succeeded|failed|ignored).",
		}, []string{"surface", "result"}),
		GlobalConflictsTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: instNamespace,
			Subsystem: instSubsystem,
			Name:      "global_conflicts_total",
			Help:      "Cross-project promotion conflicts (W6) by domain and resolution (replaced|kept_incumbent): two projects promoted contradictory actions for one trigger_key.",
		}, []string{"domain", "resolution"}),
	}
}
