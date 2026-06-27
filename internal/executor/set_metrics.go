package executor

import (
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/observability"
)

// SetMetrics updates the Prometheus metrics on a running Executor.
// Used when observability is initialised after the executor is created,
// allowing the caller to wire a new registry without re-creating the executor.
func (e *Executor) SetMetrics(metrics *Metrics) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.metrics = metrics
}

// SetHallucinationMetrics updates the Phase 1 detector metrics sink
// on a running Executor. Mirrors SetMetrics so observability can be
// initialised in any order. Nil-safe.
func (e *Executor) SetHallucinationMetrics(m *hallucination.Metrics) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hallucinationMetrics = m
}

// SetInstinctMetrics updates the instinct subsystem metrics sink on a
// running Executor, so the lead_recovery surfacing site can bump
// vornik_instinct_applications_total. Mirrors SetHallucinationMetrics:
// observability can be initialised in any order. Nil-safe.
func (e *Executor) SetInstinctMetrics(m *observability.InstinctMetrics) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.instinctMetrics = m
}
