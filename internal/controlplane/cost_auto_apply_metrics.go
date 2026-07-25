package controlplane

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CostAutoApplyMetrics are the Prometheus counters the Phase-4 auto-apply worker
// publishes (auto-apply design §6). Nil-safe: a nil *CostAutoApplyMetrics (tests /
// no observability registry) makes every inc a no-op.
type CostAutoApplyMetrics struct {
	// Results counts each per-proposal outcome by result label:
	// applied | reconciled | rejected | braked | skipped_untrusted | skipped_m1 |
	// skipped_locus | skipped_cooldown | skipped_disallowed.
	Results *prometheus.CounterVec
}

// NewCostAutoApplyMetrics registers the counter with reg (fresh registry in
// tests). Returns nil when reg is nil so callers can pass it straight through.
func NewCostAutoApplyMetrics(reg prometheus.Registerer) *CostAutoApplyMetrics {
	if reg == nil {
		return nil
	}
	return &CostAutoApplyMetrics{
		Results: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "cost_auto_apply",
			Name: "total",
			Help: "Cost/quality Phase-4 auto-apply per-proposal outcomes by result.",
		}, []string{"result"}),
	}
}

func (m *CostAutoApplyMetrics) inc(result string) {
	if m == nil || m.Results == nil {
		return
	}
	m.Results.WithLabelValues(result).Inc()
}
