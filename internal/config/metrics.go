package config

// Prometheus metrics for config hot-reload (audit R7, §3.3).
//
// The hot-reload LLD promises four operator series — reload outcome,
// validation-error count, last-success timestamp, and a staged-changes-
// pending flag — but before this file none were registered, so a failed
// reload (the case operators most need to alert on) produced no metric:
// only a WARN log line. This file registers the four series and the
// ConfigReloader observes them on every Reload() cycle.
//
// Registered names (Subsystem "config", Namespace "vornik"):
//
//	vornik_config_reload_total{status}        — counter (status=success|failure)
//	vornik_config_validation_errors           — gauge (errors in the last cycle)
//	vornik_config_last_reload_timestamp       — gauge (unix seconds of last success)
//	vornik_config_staged_changes_pending      — gauge (1 = a valid-but-unactivated stage waits)

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the config hot-reload collectors. Construct once at
// daemon boot via NewMetrics(reg) and wire onto the ConfigReloader with
// SetMetrics. All Observe* methods are nil-safe so the reloader can call
// them unconditionally even when metrics aren't wired (tests, SQLite).
type Metrics struct {
	ReloadTotal          *prometheus.CounterVec
	ValidationErrors     prometheus.Gauge
	LastReloadTimestamp  prometheus.Gauge
	StagedChangesPending prometheus.Gauge
}

// NewMetrics registers every config-reload collector against reg and
// returns the handle. Uses promauto against the injected registry so a
// duplicate registration panics loudly at boot rather than silently
// shadowing — the daemon wires this exactly once.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := promauto.With(reg)
	return &Metrics{
		ReloadTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "config",
			Name:      "reload_total",
			Help:      "Config hot-reload cycles, by outcome (success|failure).",
		}, []string{"status"}),
		ValidationErrors: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "config",
			Name:      "validation_errors",
			Help:      "Number of errors recorded in the most recent config reload cycle.",
		}),
		LastReloadTimestamp: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "config",
			Name:      "last_reload_timestamp",
			Help:      "Unix timestamp (seconds) of the most recent successful config reload.",
		}),
		StagedChangesPending: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "config",
			Name:      "staged_changes_pending",
			Help:      "1 when a loaded config stage is awaiting activation (e.g. blocked), 0 otherwise.",
		}),
	}
}

// observeReload records one completed reload cycle: bumps the
// {status} counter, sets the validation-error gauge, sets the
// staged-pending flag, and — on success — stamps the last-reload
// timestamp. Nil-safe.
func (m *Metrics) observeReload(success bool, validationErrors int, stagedPending bool, at time.Time) {
	if m == nil {
		return
	}
	status := "failure"
	if success {
		status = "success"
	}
	if m.ReloadTotal != nil {
		m.ReloadTotal.WithLabelValues(status).Inc()
	}
	if m.ValidationErrors != nil {
		m.ValidationErrors.Set(float64(validationErrors))
	}
	if m.StagedChangesPending != nil {
		if stagedPending {
			m.StagedChangesPending.Set(1)
		} else {
			m.StagedChangesPending.Set(0)
		}
	}
	if success && m.LastReloadTimestamp != nil {
		m.LastReloadTimestamp.Set(float64(at.Unix()))
	}
}
