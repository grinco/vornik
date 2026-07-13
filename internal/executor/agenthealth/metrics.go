package agenthealth

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/chat"
)

// Metrics is the prometheus surface for the agent-LLM breaker (LLD §7).
// Distinct from chat.Metrics (vornik_chat_model_health_state) so the two
// breakers are independently observable. Implements MetricsSink; a nil
// *Metrics is a no-op (the breaker still gates — LLD §12 B1).
type Metrics struct {
	// ModelHealthState is the live circuit state per model:
	// 0 closed / 1 half-open / 2 open.
	ModelHealthState *prometheus.GaugeVec
	// ModelHealthTrips counts circuit-open transitions (closed→open and
	// a half-open probe failure re-opening).
	ModelHealthTrips *prometheus.CounterVec
}

// NewMetrics constructs the agent-breaker metrics on the given registerer
// (nil → prometheus.DefaultRegisterer).
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		ModelHealthState: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "vornik",
				Subsystem: "agent",
				Name:      "model_health_state",
				Help:      "Agent-container LLM circuit-breaker state per model: 0 closed, 1 half_open, 2 open.",
			},
			[]string{"model"},
		),
		ModelHealthTrips: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "agent",
				Name:      "model_health_trips_total",
				Help:      "Agent-container LLM circuit-open transitions per model (closed→open and half-open probe failure).",
			},
			[]string{"model"},
		),
	}
}

// SetStateGauge implements MetricsSink.
func (m *Metrics) SetStateGauge(model string, state chat.CircuitState) {
	if m == nil || m.ModelHealthState == nil {
		return
	}
	m.ModelHealthState.WithLabelValues(model).Set(float64(state))
}

// IncTrips implements MetricsSink.
func (m *Metrics) IncTrips(model string) {
	if m == nil || m.ModelHealthTrips == nil {
		return
	}
	m.ModelHealthTrips.WithLabelValues(model).Inc()
}
