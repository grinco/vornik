package configrecon

// Observability for the mirror-seam normalizers (LLD §5). One counter:
//
//	vornik_config_mirror_normalized_total{normalizer}
//
// counts how often each normalizer rewrote a file at the mirror seam before the
// source write + git commit. The rel PATH stays in the structured log, NOT a
// metric label, to bound cardinality (an operator can grep the log for the
// path; the metric only needs the normalizer dimension).

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the mirror-normalization collectors. Construct once at daemon
// boot via NewMetrics(reg) on the SAME registry /metrics serves from. Inc is
// nil-safe so the mirror seam can call it unconditionally even when metrics
// aren't wired (tests, minimal harnesses).
type Metrics struct {
	NormalizedTotal *prometheus.CounterVec
}

// NewMetrics registers the mirror-normalization counter against reg (nil →
// prometheus.DefaultRegisterer) and returns the handle.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	return &Metrics{
		NormalizedTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "config",
			Name:      "mirror_normalized_total",
			Help:      "Count of mirror-seam canonical-field normalizations, by normalizer. The rel path is in the structured log (not a label) to bound cardinality.",
		}, []string{"normalizer"}),
	}
}

// Inc records one normalization by the named normalizer. Nil-safe.
func (m *Metrics) Inc(normalizer string) {
	if m == nil || m.NormalizedTotal == nil {
		return
	}
	m.NormalizedTotal.WithLabelValues(normalizer).Inc()
}
