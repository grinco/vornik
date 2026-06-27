package extractor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	extNamespace = "vornik"
	extSubsystem = "" // metrics carry the full name in Name (no subsystem prefix)
)

// Metrics holds the Prometheus metrics for the document-extraction
// pipeline. They live behind the Runner — the single completion seam
// every extraction flows through — so HTTP-triggered, workflow-step,
// and email-channel extractions all land on the same series.
//
// All fields are emitted via the nil-safe helper methods below, so a
// Runner built without Metrics (tests, ArtifactsPath-unset) simply
// doesn't emit.
//
// Closes the document-extraction LLD Phase 7 metrics item.
type Metrics struct {
	// ExtractionsTotal counts extraction attempts by extractor and
	// outcome: ok | error. "error" covers every post-Extract failure
	// path (extractor error, empty result, on-disk write, persistence).
	ExtractionsTotal *prometheus.CounterVec

	// ExtractionDurationSeconds is the per-extraction wall time of the
	// Extract call itself (parse + section emit), labelled by extractor.
	// Observed for both ok and error outcomes — the work happened either
	// way.
	ExtractionDurationSeconds *prometheus.HistogramVec

	// ExtractedDocumentsTotal counts successfully-persisted extracted
	// documents by project, the throughput operators watch per tenant.
	ExtractedDocumentsTotal *prometheus.CounterVec
}

// NewMetrics registers and returns the extraction pipeline metrics. A
// nil registerer falls back to the default Prometheus registerer.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	pa := promauto.With(registerer)
	return &Metrics{
		ExtractionsTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: extNamespace,
			Subsystem: extSubsystem,
			Name:      "extractions_total",
			Help:      "Document extraction attempts by extractor and outcome (ok|error).",
		}, []string{"extractor", "status"}),
		ExtractionDurationSeconds: pa.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: extNamespace,
			Subsystem: extSubsystem,
			Name:      "extraction_duration_seconds",
			Help:      "Per-extraction Extract() wall time in seconds, by extractor.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"extractor"}),
		ExtractedDocumentsTotal: pa.NewCounterVec(prometheus.CounterOpts{
			Namespace: extNamespace,
			Subsystem: extSubsystem,
			Name:      "extracted_documents_total",
			Help:      "Successfully persisted extracted documents by project.",
		}, []string{"project"}),
	}
}

// recordExtraction bumps the outcome counter. Nil-safe: a nil *Metrics
// (Runner without metrics) is a no-op.
func (m *Metrics) recordExtraction(extractor, status string) {
	if m == nil || m.ExtractionsTotal == nil {
		return
	}
	m.ExtractionsTotal.WithLabelValues(extractor, status).Inc()
}

// observeDuration records the Extract() latency in seconds. Nil-safe.
func (m *Metrics) observeDuration(extractor string, seconds float64) {
	if m == nil || m.ExtractionDurationSeconds == nil {
		return
	}
	m.ExtractionDurationSeconds.WithLabelValues(extractor).Observe(seconds)
}

// recordDocument bumps the per-project persisted-document counter.
// Nil-safe.
func (m *Metrics) recordDocument(projectID string) {
	if m == nil || m.ExtractedDocumentsTotal == nil {
		return
	}
	m.ExtractedDocumentsTotal.WithLabelValues(projectID).Inc()
}
