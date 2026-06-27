package chat

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds Prometheus metrics for the chat/LLM client.
//
// Queue* fields cover the shared bounded worker pool installed by
// NewQueuedProvider when chat.max_concurrent_requests > 0. They are
// global (unlabelled) because the queue is per-daemon, not per-model
// — when calls fan into the queue we've already lost the model
// distinction; what matters is whether the queue itself is the
// bottleneck. The QueueCallsTotal counter carries an "outcome" label
// (started|canceled) so operators can read the cancel rate without
// scraping logs.
type Metrics struct {
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	TokensUsed      *prometheus.CounterVec
	ErrorsTotal     *prometheus.CounterVec

	QueueDepth       prometheus.Gauge
	QueueInFlight    prometheus.Gauge
	QueueWaitSeconds prometheus.Histogram
	QueueCallsTotal  *prometheus.CounterVec

	// Prompt-cache observability (audit N8, §3.3). The per-call cache
	// token counts already land in task_llm_usage and the spend UI, but
	// no Prometheus series exposed them — operators tuning cache
	// breakpoints had a DB query, not a Grafana panel. Labelled by
	// model + role (dispatcher / external_api / a workflow step role) +
	// source (external_api / workflow_step) so the cache rate can be
	// split by traffic class.
	CacheCreationTokensTotal *prometheus.CounterVec
	CacheReadTokensTotal     *prometheus.CounterVec
	// CacheHitRatio is the cumulative cache-read share of cacheable
	// input tokens (read / (read + creation)) for a (model, role)
	// pair, set as a gauge from running totals. A ratio near 1.0 means
	// the prompt prefix is being reused efficiently; a collapse toward
	// 0 flags a cache-busting prompt change.
	CacheHitRatio *prometheus.GaugeVec
	// CacheDollarsSavedTotal accumulates the USD value of serving input
	// tokens from cache instead of at full input rate, per (model,
	// role). Computed from the pricing table at observation time.
	CacheDollarsSavedTotal *prometheus.CounterVec

	// SubscriptionTokenRefreshTotal counts OAuth token-refresh
	// outcomes for the subscription providers (2026-06-07 review:
	// refresh failures previously collapsed into generic request
	// errors with no breakdown, and codex quarantine transitions
	// were invisible). provider = claude | codex; outcome =
	// success | failure | invalid_grant_recovered (claude adopted a
	// concurrent CLI's on-disk rotation) | quarantined (codex dead
	// refresh token — only a re-login recovers).
	SubscriptionTokenRefreshTotal *prometheus.CounterVec

	cacheRatioMu    sync.Mutex
	cacheRatioState map[string]*cacheRatioState
}

// RecordSubscriptionTokenRefresh increments the token-refresh outcome
// counter. Nil-safe on the receiver so auth managers can call it
// unconditionally (metrics are wired only in full-daemon setups).
func (m *Metrics) RecordSubscriptionTokenRefresh(provider, outcome string) {
	if m == nil || m.SubscriptionTokenRefreshTotal == nil {
		return
	}
	m.SubscriptionTokenRefreshTotal.WithLabelValues(provider, outcome).Inc()
}

// cacheRatioState tracks the running read/creation token totals per
// (model, role) so CacheHitRatio can be recomputed on each observation
// without re-scraping. Guarded by cacheRatioMu.
type cacheRatioState struct {
	read     float64
	creation float64
}

// NewMetrics creates and registers chat metrics.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "requests_total",
			Help:      "Total LLM chat completion requests.",
		}, []string{"model", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "request_duration_seconds",
			Help:      "LLM chat completion request duration.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"model"}),
		TokensUsed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "tokens_used_total",
			Help:      "Total LLM tokens consumed.",
		}, []string{"model", "type"}), // type: prompt, completion
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "errors_total",
			Help:      "Total LLM chat completion errors.",
		}, []string{"model", "error_type"}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "queue_depth",
			Help:      "Number of LLM calls currently waiting in the priority queue.",
		}),
		QueueInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "queue_in_flight",
			Help:      "Number of LLM calls currently executing (popped from the queue, not yet returned).",
		}),
		QueueWaitSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "queue_wait_seconds",
			Help:      "Time an LLM call spent waiting in the priority queue before a worker picked it up.",
			Buckets:   []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120},
		}),
		QueueCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "queue_calls_total",
			Help:      "LLM calls that left the priority queue, by outcome (started=worker picked up; canceled=ctx done before start).",
		}, []string{"outcome"}),
		CacheCreationTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "cache_creation_tokens_total",
			Help:      "Input tokens written to the provider prompt cache, by model, role and source.",
		}, []string{"model", "role", "source"}),
		CacheReadTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "cache_read_tokens_total",
			Help:      "Input tokens served from the provider prompt cache, by model, role and source.",
		}, []string{"model", "role", "source"}),
		CacheHitRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "cache_hit_ratio",
			Help:      "Cumulative cache-read share of cacheable input tokens (read/(read+creation)), by model and role.",
		}, []string{"model", "role"}),
		CacheDollarsSavedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "cache_dollars_saved_total",
			Help:      "Cumulative USD saved by serving input tokens from cache instead of at full input rate, by model and role.",
		}, []string{"model", "role"}),
		SubscriptionTokenRefreshTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "chat",
			Name:      "subscription_token_refresh_total",
			Help: "Subscription-provider OAuth token-refresh outcomes. outcome = success | failure | " +
				"invalid_grant_recovered (claude adopted a concurrent CLI's rotation) | quarantined (codex dead token).",
		}, []string{"provider", "outcome"}),
		cacheRatioState: map[string]*cacheRatioState{},
	}
	reg.MustRegister(
		m.RequestsTotal, m.RequestDuration, m.TokensUsed, m.ErrorsTotal,
		m.QueueDepth, m.QueueInFlight, m.QueueWaitSeconds, m.QueueCallsTotal,
		m.CacheCreationTokensTotal, m.CacheReadTokensTotal,
		m.CacheHitRatio, m.CacheDollarsSavedTotal,
		m.SubscriptionTokenRefreshTotal,
	)
	return m
}

// ObserveCacheUsage records one chat call's prompt-cache token usage:
// it adds the creation/read token counts to their counters (labelled
// model/role/source), recomputes the running cache-hit ratio gauge for
// the (model, role) pair, and adds dollarsSaved to the savings counter.
//
// Nil-safe: the api chat-proxy / llm-usage handler call this on every
// recorded usage row, including in deployments / tests where chat
// metrics were never wired.
func (m *Metrics) ObserveCacheUsage(model, role, source string, creationTokens, readTokens int64, dollarsSaved float64) {
	if m == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if role == "" {
		role = "unknown"
	}
	if source == "" {
		source = "unknown"
	}
	if m.CacheCreationTokensTotal != nil && creationTokens > 0 {
		m.CacheCreationTokensTotal.WithLabelValues(model, role, source).Add(float64(creationTokens))
	}
	if m.CacheReadTokensTotal != nil && readTokens > 0 {
		m.CacheReadTokensTotal.WithLabelValues(model, role, source).Add(float64(readTokens))
	}
	if m.CacheDollarsSavedTotal != nil && dollarsSaved > 0 {
		m.CacheDollarsSavedTotal.WithLabelValues(model, role).Add(dollarsSaved)
	}
	// Recompute the (model, role) hit ratio from running totals. The
	// gauge is a ratio, not a cumulative sum, so it can't be derived
	// from the counters alone without a PromQL rate() — we keep the
	// running totals here so the panel reads a stable lifetime ratio.
	if m.CacheHitRatio != nil && (creationTokens > 0 || readTokens > 0) {
		key := model + "\x00" + role
		m.cacheRatioMu.Lock()
		st := m.cacheRatioState[key]
		if st == nil {
			st = &cacheRatioState{}
			m.cacheRatioState[key] = st
		}
		st.creation += float64(creationTokens)
		st.read += float64(readTokens)
		denom := st.read + st.creation
		var ratio float64
		if denom > 0 {
			ratio = st.read / denom
		}
		m.cacheRatioMu.Unlock()
		m.CacheHitRatio.WithLabelValues(model, role).Set(ratio)
	}
}
