package runtime

import "github.com/prometheus/client_golang/prometheus"

// PoolMetrics holds Prometheus metrics for the warm pool.
type PoolMetrics struct {
	PoolSize      *prometheus.GaugeVec
	WarmHits      *prometheus.CounterVec
	ColdStarts    *prometheus.CounterVec
	IdleEvictions *prometheus.CounterVec
}

// NewPoolMetrics creates and registers pool metrics.
func NewPoolMetrics(reg *prometheus.Registry) *PoolMetrics {
	m := &PoolMetrics{
		PoolSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "warm_pool",
			Name:      "size",
			Help:      "Current number of warm containers by project and role.",
		}, []string{"project_id", "role"}),
		WarmHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "warm_pool",
			Name:      "warm_hits_total",
			Help:      "Number of times an existing warm container was reused.",
		}, []string{"project_id", "role"}),
		ColdStarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "warm_pool",
			Name:      "cold_starts_total",
			Help:      "Number of times no warm container was available (cold start).",
		}, []string{"project_id", "role"}),
		IdleEvictions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "warm_pool",
			Name:      "idle_evictions_total",
			Help:      "Number of warm containers evicted due to idle timeout.",
		}, []string{"project_id", "role"}),
	}
	reg.MustRegister(m.PoolSize, m.WarmHits, m.ColdStarts, m.IdleEvictions)
	return m
}
