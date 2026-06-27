package api

import "github.com/prometheus/client_golang/prometheus"

// TradingMetrics holds Prometheus counters for the broker→daemon
// audit-channel ingest path. The broker MCP posts one row per
// place_order call (success or refused) and one row per safety
// envelope decision (kill-switch toggle, breaker trip, cap
// refusal, idempotency replay hit). Both endpoints are idempotent
// on id at the DB level; the counters here track *attempts* — i.e.
// every ingest call regardless of whether it inserted a new row —
// so a sudden burst of retries shows up too.
type TradingMetrics struct {
	// SafetyEventsTotal counts every safety-event ingest call. The
	// `kind` label uses the broker's vocabulary (kill_switch_on/
	// off, breaker_trip, cap_refused, replay_hit) so dashboards
	// can split by event class without parsing the JSONB detail
	// column.
	SafetyEventsTotal *prometheus.CounterVec
	// OrdersIngestedTotal counts every order-ingest call. `status`
	// is "placed" or "refused" (taken from the order row's
	// status field where the broker records its envelope
	// decision).
	OrdersIngestedTotal *prometheus.CounterVec
	// FillsIngestedTotal counts every fill-ingest call. One row
	// per partial fill, so a four-leg fill on a single order
	// shows up as four increments.
	FillsIngestedTotal *prometheus.CounterVec
	// IngestErrorsTotal counts ingest calls that failed before
	// persistence (validation, oversized body, auth). Distinct
	// from PERSIST_FAILED 5xxs — those land on the DB-side error
	// rate. Split by endpoint so an operator can see which
	// channel is misbehaving without correlating the request log.
	IngestErrorsTotal *prometheus.CounterVec
}

// NewTradingMetrics registers the trading ingest metrics on the
// provided registry.
func NewTradingMetrics(reg prometheus.Registerer) *TradingMetrics {
	m := &TradingMetrics{
		SafetyEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "trading",
			Name:      "safety_events_total",
			Help:      "Total trading safety envelope events ingested from the broker.",
		}, []string{"project_id", "kind", "severity"}),
		OrdersIngestedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "trading",
			Name:      "orders_ingested_total",
			Help:      "Total trading orders ingested from the broker, by placement status.",
		}, []string{"project_id", "status"}),
		FillsIngestedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "trading",
			Name:      "fills_ingested_total",
			Help:      "Total trading fill events ingested from the broker.",
		}, []string{"project_id"}),
		IngestErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "trading",
			Name:      "ingest_errors_total",
			Help:      "Trading audit-channel ingest errors (validation, body too large, auth).",
		}, []string{"endpoint", "reason"}),
	}
	if reg != nil {
		reg.MustRegister(
			m.SafetyEventsTotal,
			m.OrdersIngestedTotal,
			m.FillsIngestedTotal,
			m.IngestErrorsTotal,
		)
	}
	return m
}
