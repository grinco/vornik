package telegram

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the Telegram bot.
type Metrics struct {
	MessagesReceived *prometheus.CounterVec
	MessagesSent     *prometheus.CounterVec
	ToolCallsTotal   *prometheus.CounterVec
	RateLimitsHit    prometheus.Counter
	ActiveSessions   prometheus.Gauge
	MessageLatency   *prometheus.HistogramVec
}

// NewMetrics creates and registers Telegram metrics.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		MessagesReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "messages_received_total",
			Help:      "Total messages received from Telegram users.",
		}, []string{"user_id"}),
		MessagesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "messages_sent_total",
			Help:      "Total messages sent to Telegram users.",
		}, []string{"status"}), // success, error
		ToolCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "tool_calls_total",
			Help:      "Total tool calls executed by the bot.",
		}, []string{"tool"}),
		RateLimitsHit: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "rate_limits_hit_total",
			Help:      "Total messages rejected due to rate limiting.",
		}),
		ActiveSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "active_sessions",
			Help:      "Current number of active conversation sessions.",
		}),
		MessageLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "telegram",
			Name:      "message_processing_seconds",
			Help:      "Time to process a user message (LLM call + tool execution).",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
		}, []string{"user_id"}),
	}
	reg.MustRegister(m.MessagesReceived, m.MessagesSent, m.ToolCallsTotal,
		m.RateLimitsHit, m.ActiveSessions, m.MessageLatency)
	return m
}
