package runtime

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	runtimeNamespace = "vornik"
	runtimeSubsystem = "runtime"
)

// Metrics holds the Prometheus metrics for the runtime package.
type Metrics struct {
	// ContainersStartedTotal is a counter tracking containers started.
	ContainersStartedTotal *prometheus.CounterVec

	// ContainersStoppedTotal is a counter tracking containers stopped.
	ContainersStoppedTotal *prometheus.CounterVec

	// ContainersRunning is a gauge tracking currently running containers.
	ContainersRunning *prometheus.GaugeVec

	// ContainerStartLatencySeconds is a histogram tracking time to start container.
	ContainerStartLatencySeconds *prometheus.HistogramVec

	// ContainerStopLatencySeconds is a histogram tracking time to stop container.
	ContainerStopLatencySeconds *prometheus.HistogramVec

	// PodmanErrorsTotal is a counter tracking Podman CLI failures by operation.
	PodmanErrorsTotal *prometheus.CounterVec

	// ContainerOOMKillsTotal counts containers killed for exceeding their
	// memory limit, by role.
	//
	// The closed-loop signal for the limit introduced on 2026-09-21. The
	// limit's default is DERIVED from host memory and concurrency, and its
	// headroom share is a policy constant rather than a measurement — so
	// "the default is too low" has to be observable rather than something an
	// operator discovers from a task that stopped working. Attributability
	// after the fact plus a one-line config change is a mitigation only with
	// a human already looking.
	//
	// Labelled by ROLE and not by task: a counter showing one role OOMing
	// while the others never do is the evidence that reopens per-role limits,
	// which are deliberately deferred until it exists.
	ContainerOOMKillsTotal *prometheus.CounterVec

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
		ContainersStartedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "containers_started_total",
				Help:      "Total number of containers started.",
			},
			[]string{"project_id"},
		),
		ContainersStoppedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "containers_stopped_total",
				Help:      "Total number of containers stopped.",
			},
			[]string{"project_id"},
		),
		ContainersRunning: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "containers_running",
				Help:      "Number of currently running containers.",
			},
			[]string{"project_id"},
		),
		ContainerStartLatencySeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "container_start_latency_seconds",
				Help:      "Time to start container in seconds.",
				// Buckets optimized for container start: 100ms to 2 minutes
				Buckets: []float64{
					0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120,
				},
			},
			[]string{"project_id"},
		),
		ContainerStopLatencySeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "container_stop_latency_seconds",
				Help:      "Time to stop container in seconds.",
				// Buckets optimized for container stop: 100ms to 2 minutes
				Buckets: []float64{
					0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120,
				},
			},
			[]string{"project_id"},
		),
		PodmanErrorsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "podman_errors_total",
				Help:      "Total number of Podman CLI failures by operation.",
			},
			[]string{"operation"},
		),
		ContainerOOMKillsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: runtimeNamespace,
				Subsystem: runtimeSubsystem,
				Name:      "container_oom_kills_total",
				Help:      "Containers killed for exceeding their memory limit, by role.",
			},
			[]string{"role"},
		),
	}

	return m
}

// RecordContainerOOMKill counts one container killed for exceeding its memory
// limit.
//
// Exit 137 is the signal: SIGKILL delivered by the kernel's cgroup OOM
// handling. It is deliberately NOT inferred from the exit code alone anywhere
// else — 137 can also be an operator kill — so the caller decides, and this
// only counts.
func (m *Metrics) RecordContainerOOMKill(role string) {
	if m == nil || m.ContainerOOMKillsTotal == nil {
		return
	}
	if role == "" {
		role = "unknown"
	}
	m.ContainerOOMKillsTotal.WithLabelValues(role).Inc()
}

// RecordContainerStarted increments the containers started counter and running gauge.
func (m *Metrics) RecordContainerStarted(projectID string, latencySeconds float64) {
	if m == nil {
		return
	}
	m.ContainersStartedTotal.WithLabelValues(projectID).Inc()
	m.ContainersRunning.WithLabelValues(projectID).Inc()
	m.ContainerStartLatencySeconds.WithLabelValues(projectID).Observe(latencySeconds)
}

// RecordContainerStopped increments the containers stopped counter and decrements running gauge.
func (m *Metrics) RecordContainerStopped(projectID string, latencySeconds float64) {
	if m == nil {
		return
	}
	m.ContainersStoppedTotal.WithLabelValues(projectID).Inc()
	m.ContainersRunning.WithLabelValues(projectID).Dec()
	m.ContainerStopLatencySeconds.WithLabelValues(projectID).Observe(latencySeconds)
}

// RecordPodmanError increments the Podman errors counter for the given operation.
func (m *Metrics) RecordPodmanError(operation string) {
	if m == nil {
		return
	}
	m.PodmanErrorsTotal.WithLabelValues(operation).Inc()
}

// SetContainersRunning sets the running containers gauge to a specific value.
// This is useful for initialization or reconciliation.
func (m *Metrics) SetContainersRunning(projectID string, count float64) {
	if m == nil {
		return
	}
	m.ContainersRunning.WithLabelValues(projectID).Set(count)
}
