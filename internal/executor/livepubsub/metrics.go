package livepubsub

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the live-event publisher
// (live-task-observation LLD §10). published is counted at the local
// publish seam; dropped is counted when a non-blocking fan-out skips a
// slow subscriber. Both are nil-safe — a nil *Metrics disables emission.
type Metrics struct {
	PublishedTotal *prometheus.CounterVec // {kind}
	DroppedTotal   *prometheus.CounterVec // {reason}
}

// NewMetrics creates and registers the live-event metrics. Returns nil
// when reg is nil (observability disabled) — callers and the publisher
// nil-check, so this stays a no-op rather than registering on a phantom
// registry. Takes the concrete *prometheus.Registry (not the Registerer
// interface) to avoid the typed-nil trap a nil registry would otherwise
// hide behind a non-nil interface value.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		PublishedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "live",
			Name:      "events_published_total",
			Help:      "Live execution events published, by event kind.",
		}, []string{"kind"}),
		DroppedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "live",
			Name:      "events_dropped_total",
			Help:      "Live execution events dropped to a slow subscriber during non-blocking fan-out, by reason.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.PublishedTotal, m.DroppedTotal)
	return m
}
