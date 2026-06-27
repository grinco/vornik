package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"vornik.io/vornik/internal/persistence"
)

const (
	schedulerNamespace = "vornik"
	schedulerSubsystem = "scheduler"
)

// Metrics holds the Prometheus metrics for the scheduler package.
type Metrics struct {
	// LeasesAcquiredTotal is a counter tracking tasks leased by the scheduler.
	LeasesAcquiredTotal *prometheus.CounterVec

	// LeasesExpiredTotal is a counter tracking leases that expired.
	LeasesExpiredTotal *prometheus.CounterVec

	// RecoveriesTotal is a counter tracking expired lease recoveries.
	RecoveriesTotal *prometheus.CounterVec

	// LeaseDurationSeconds is a histogram tracking time to acquire lease.
	LeaseDurationSeconds *prometheus.HistogramVec

	// QueueWaitSeconds is a histogram tracking queue residency — the time a
	// task spends QUEUED before the scheduler leases it (creation → lease).
	// This used to live on the queue package (vornik_queue_latency_seconds)
	// but the scheduler took over leasing directly off the repo, leaving the
	// queue's Lease() — and therefore that metric — dead (never emitted).
	// Ownership now matches reality: the scheduler observes queue wait at the
	// point it actually acquires the lease.
	QueueWaitSeconds *prometheus.HistogramVec

	// TasksScheduledTotal is a counter tracking tasks handed to executor.
	TasksScheduledTotal *prometheus.CounterVec

	// QueueDepthGauge tracks the number of tasks in the queue by status.
	QueueDepthGauge *prometheus.GaugeVec

	// ExecutionsCompletedTotal is a counter tracking completed executions.
	ExecutionsCompletedTotal *prometheus.CounterVec

	// ExecutionLatencySeconds is a histogram tracking execution duration.
	ExecutionLatencySeconds *prometheus.HistogramVec

	// LoopsTotal counts scheduler loop iterations — one per scheduling
	// pass (a poll-interval tick or an operator Wake()). The scheduler
	// LLD §8 promises vornik_scheduler_loops_total but no loop-iteration
	// counter existed; without it operators couldn't tell a wedged loop
	// (no increments) from an idle-but-healthy one (audit R5). Unlabelled
	// — the loop is per-daemon, not per-project.
	LoopsTotal prometheus.Counter

	// registry is the Prometheus registerer used for registering metrics.
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
		LeasesAcquiredTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "leases_acquired_total",
				Help:      "Total number of tasks leased by the scheduler.",
			},
			[]string{"project_id"},
		),
		LeasesExpiredTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "leases_expired_total",
				Help:      "Total number of leases that expired.",
			},
			[]string{"project_id"},
		),
		RecoveriesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "recoveries_total",
				Help:      "Total number of expired lease recoveries.",
			},
			[]string{"project_id"},
		),
		LeaseDurationSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "lease_duration_seconds",
				Help:      "Time to acquire lease in seconds.",
				// Buckets optimized for lease acquisition: 1ms to 30 seconds
				Buckets: []float64{
					0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
					1, 2.5, 5, 10, 30,
				},
			},
			[]string{"project_id"},
		),
		TasksScheduledTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "tasks_scheduled_total",
				Help:      "Total number of tasks handed to executor.",
			},
			[]string{"project_id"},
		),
		QueueWaitSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "queue_wait_seconds",
				Help:      "Time tasks spend QUEUED before the scheduler leases them (queue residency).",
				// Buckets optimized for queue residency: 10ms to 5 minutes.
				Buckets: []float64{
					0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
					1, 2.5, 5, 10, 30, 60, 120, 300,
				},
			},
			[]string{"project_id"},
		),
		QueueDepthGauge: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "queue_depth",
				Help:      "Number of tasks in the queue by status.",
			},
			[]string{"project_id", "status"},
		),
		ExecutionsCompletedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "executions_completed_total",
				Help:      "Total number of completed executions.",
			},
			[]string{"project_id", "status"},
		),
		ExecutionLatencySeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "execution_latency_seconds",
				Help:      "Time spent executing tasks in seconds.",
				// Buckets optimized for task execution: 100ms to 30 minutes
				Buckets: []float64{
					0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800,
				},
			},
			[]string{"project_id", "status"},
		),
		LoopsTotal: promauto.With(registerer).NewCounter(
			prometheus.CounterOpts{
				Namespace: schedulerNamespace,
				Subsystem: schedulerSubsystem,
				Name:      "loops_total",
				Help:      "Total scheduler loop iterations (one per scheduling pass: poll-interval tick or Wake()).",
			},
		),
	}

	return m
}

// RecordLoop increments the scheduler loop-iteration counter — called
// once per scheduling pass. Nil-safe.
func (m *Metrics) RecordLoop() {
	if m == nil || m.LoopsTotal == nil {
		return
	}
	m.LoopsTotal.Inc()
}

// RecordLeaseAcquired increments the leases acquired counter.
func (m *Metrics) RecordLeaseAcquired(projectID string) {
	if m == nil {
		return
	}
	m.LeasesAcquiredTotal.WithLabelValues(projectID).Inc()
}

// RecordLeaseExpired increments the leases expired counter.
func (m *Metrics) RecordLeaseExpired(projectID string) {
	if m == nil {
		return
	}
	m.LeasesExpiredTotal.WithLabelValues(projectID).Inc()
}

// RecordRecovery increments the recoveries counter.
func (m *Metrics) RecordRecovery(projectID string) {
	if m == nil {
		return
	}
	m.RecoveriesTotal.WithLabelValues(projectID).Inc()
}

// RecordLeaseDuration records the time taken to acquire a lease.
func (m *Metrics) RecordLeaseDuration(projectID string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.LeaseDurationSeconds.WithLabelValues(projectID).Observe(durationSeconds)
}

// RecordQueueWait records queue residency — the time a task spent QUEUED
// before being leased. Skipped when the task carries no creation timestamp.
func (m *Metrics) RecordQueueWait(projectID string, waitSeconds float64) {
	if m == nil || m.QueueWaitSeconds == nil {
		return
	}
	m.QueueWaitSeconds.WithLabelValues(projectID).Observe(waitSeconds)
}

// RecordTaskScheduled increments the tasks scheduled counter.
func (m *Metrics) RecordTaskScheduled(projectID string) {
	if m == nil {
		return
	}
	m.TasksScheduledTotal.WithLabelValues(projectID).Inc()
}

// UpdateQueueDepth updates the queue depth gauge for a project and status.
func (m *Metrics) UpdateQueueDepth(projectID string, status persistence.TaskStatus, count int64) {
	if m == nil {
		return
	}
	m.QueueDepthGauge.WithLabelValues(projectID, string(status)).Set(float64(count))
}

// RecordExecutionCompleted records a completed execution with its latency.
func (m *Metrics) RecordExecutionCompleted(projectID string, status string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.ExecutionsCompletedTotal.WithLabelValues(projectID, status).Inc()
	m.ExecutionLatencySeconds.WithLabelValues(projectID, status).Observe(durationSeconds)
}
