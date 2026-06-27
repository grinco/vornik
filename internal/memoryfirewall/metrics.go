package memoryfirewall

// Prometheus metrics for the Policy-Aware Memory Firewall.
//
// https://docs.vornik.io §
// "Observability" promises six metric series. Before the 2026-05-29
// LLD-drift fix (§8.3) this file did not exist — the AuditWriter
// carried an AuditMetrics hook struct but nobody constructed the
// collectors or wired them, so every promised series read flat zero
// and operators wiring Grafana panels saw nothing.
//
// The six series (names verbatim from the LLD):
//
//	memory_firewall_decisions_total{project, decision}     — counter
//	memory_firewall_eval_duration_seconds{project}         — histogram
//	memory_firewall_audit_buffer_depth                     — gauge
//	memory_firewall_audit_writes_total{result}             — counter
//	memory_firewall_audit_write_latency_seconds            — histogram
//	memory_firewall_chunks_by_sensitivity{project, tier}   — gauge (swept)
//
// The four audit-writer series feed AuditWriter via AuditMetrics
// (its existing nil-safe hook struct). The two recall-side series
// (decisions + eval_duration) are observed by the Searcher's
// firewall pass. chunks_by_sensitivity is a gauge a periodic sweep
// sets from a chunk-count query.

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	mfNamespace = "memory_firewall"
)

// Metrics holds the firewall's Prometheus collectors. Construct once
// at daemon boot via NewMetrics(reg); pass AuditMetrics() to
// AuditWriter.SetMetrics and call ObserveDecision / ObserveEval on
// the recall path.
type Metrics struct {
	decisions      *prometheus.CounterVec
	evalDuration   *prometheus.HistogramVec
	auditBufferDep prometheus.Gauge
	auditWrites    *prometheus.CounterVec
	auditWriteLat  prometheus.Histogram
	chunksBySens   *prometheus.GaugeVec
}

// NewMetrics registers every firewall collector against reg and
// returns the handle. Registration uses promauto so a duplicate
// registration panics loudly at boot rather than silently shadowing
// — the daemon wires this exactly once (rebuildSchedulerMetrics).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		decisions: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: mfNamespace,
			Name:      "decisions_total",
			Help:      "Per-chunk firewall evaluation decisions, by project and decision class.",
		}, []string{"project", "decision"}),
		evalDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: mfNamespace,
			Name:      "eval_duration_seconds",
			Help:      "Wall-clock time to evaluate one recall's chunk set through the firewall.",
			// Sub-millisecond to a few ms: the evaluator is in-process
			// Go with one batched policy SELECT. Buckets centred there.
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
		}, []string{"project"}),
		auditBufferDep: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: mfNamespace,
			Name:      "audit_buffer_depth",
			Help:      "Current depth of the audit writer's in-memory batch buffer.",
		}),
		auditWrites: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: mfNamespace,
			Name:      "audit_writes_total",
			Help:      "Audit batch-insert attempts, by result (ok|error).",
		}, []string{"result"}),
		auditWriteLat: factory.NewHistogram(prometheus.HistogramOpts{
			Namespace: mfNamespace,
			Name:      "audit_write_latency_seconds",
			Help:      "Latency of one audit batch insert.",
			Buckets:   prometheus.DefBuckets,
		}),
		chunksBySens: factory.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: mfNamespace,
			Name:      "chunks_by_sensitivity",
			Help:      "Count of chunks per sensitivity tier, per project (set by a periodic sweep).",
		}, []string{"project", "tier"}),
	}
}

// ObserveDecision bumps memory_firewall_decisions_total. Nil-safe so
// the recall path can call unconditionally even when metrics aren't
// wired (SQLite / tests).
func (m *Metrics) ObserveDecision(projectID string, decision EvaluationDecision) {
	if m == nil {
		return
	}
	m.decisions.WithLabelValues(projectID, string(decision)).Inc()
}

// ObserveEval records memory_firewall_eval_duration_seconds for one
// recall's firewall pass. Nil-safe.
func (m *Metrics) ObserveEval(projectID string, d time.Duration) {
	if m == nil {
		return
	}
	m.evalDuration.WithLabelValues(projectID).Observe(d.Seconds())
}

// SetChunksBySensitivity sets the swept gauge for one (project, tier)
// pair. Called by the periodic sweep, not the hot path. Nil-safe.
func (m *Metrics) SetChunksBySensitivity(projectID, tier string, count float64) {
	if m == nil {
		return
	}
	m.chunksBySens.WithLabelValues(projectID, tier).Set(count)
}

// AuditMetrics returns the hook struct the AuditWriter consumes via
// SetMetrics, bound to this Metrics' collectors. Nil receiver yields
// nil so the writer's own nil-checks skip cleanly.
func (m *Metrics) AuditMetrics() *AuditMetrics {
	if m == nil {
		return nil
	}
	return &AuditMetrics{
		BufferDepth:  func(n int) { m.auditBufferDep.Set(float64(n)) },
		WritesTotal:  func(result string) { m.auditWrites.WithLabelValues(result).Inc() },
		WriteLatency: func(d time.Duration) { m.auditWriteLat.Observe(d.Seconds()) },
		DroppedTotal: func() { m.auditWrites.WithLabelValues("dropped").Inc() },
	}
}
