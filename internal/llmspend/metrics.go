package llmspend

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "vornik"
	metricSubsystem = "llm_usage"
)

// prometheusFailureSink counts ledger writes that failed, labelled by source.
//
// This metric is the reason Record can swallow its error honestly. Before the
// seam existed, nineteen call sites each swallowed it with `_ = repo.Record(...)`
// and there was nowhere to hang a counter — so a systematic write failure looked
// exactly like a quiet period. One place means one metric.
//
// It is only worth having if it is ALARMED: three unbilled call sites reached
// production and every one was found by an operator reading a bill, not by anyone
// noticing a dashboard. The alert lives with the Cost & Observability dashboard
// in deployments/grafana/dashboards/cost.json.
type prometheusFailureSink struct {
	failures *prometheus.CounterVec
}

// NewPrometheusFailureSink registers the counter and returns a FailureSink.
// Pass nil to use the default registerer, matching the convention in
// internal/memory/metrics.go.
func NewPrometheusFailureSink(registerer prometheus.Registerer) FailureSink {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	// Register-or-reuse rather than promauto's register-or-panic. The counter is
	// process-global by nature, but a container is not: a test binary that builds
	// several containers would otherwise panic on the second one with "duplicate
	// metrics collector registration attempted" — which it did, immediately.
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			// Namespace + Subsystem + Name compose to
			// vornik_llm_usage_record_failures_total. Split this way to match the
			// convention in internal/memory/metrics.go, which is also the shape
			// the LLD metric linter resolves.
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "record_failures_total",
			Help: "Ledger writes to task_llm_usage that failed, by source. " +
				"Non-zero means real LLM spend happened and was not attributed. " +
				"The provider already charged, so these dollars are unrecoverable " +
				"from the ledger and must be reconciled from the provider bill.",
		},
		[]string{"source"},
	)
	if err := registerer.Register(counter); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
				return &prometheusFailureSink{failures: existing}
			}
		}
		// Any other registration failure: return a sink that counts nothing rather
		// than failing startup. Losing the metric is bad; refusing to boot over it
		// is worse.
		return &prometheusFailureSink{}
	}
	return &prometheusFailureSink{failures: counter}
}

func (p *prometheusFailureSink) Inc(source string) {
	if p == nil || p.failures == nil {
		return
	}
	p.failures.WithLabelValues(source).Inc()
}
