package queue

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	namespace = "vornik"
	subsystem = "queue"
)

// Metrics holds the Prometheus metrics for the queue package.
type Metrics struct {
	// Depth is a gauge tracking the current number of pending tasks in the queue.
	Depth *prometheus.GaugeVec

	// EnqueuedTotal is a counter tracking the total number of tasks enqueued.
	EnqueuedTotal *prometheus.CounterVec

	// DLQTotal is a counter tracking the total number of tasks moved to the dead letter queue.
	DLQTotal *prometheus.CounterVec

	// registry is the Prometheus registry used for registering metrics.
	registry prometheus.Registerer
}

// NewMetrics creates a new Metrics instance with the given Prometheus registerer.
// If registerer is nil, prometheus.DefaultRegisterer is used.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	m := &Metrics{
		registry: registerer,
		Depth: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "depth",
				Help:      "Current number of pending tasks in the queue.",
			},
			[]string{"project_id"},
		),
		EnqueuedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "enqueued_total",
				Help:      "Total number of tasks enqueued.",
			},
			[]string{"project_id"},
		),
		DLQTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "dlq_total",
				Help:      "Total number of tasks moved to the dead letter queue.",
			},
			[]string{"project_id"},
		),
	}

	return m
}
