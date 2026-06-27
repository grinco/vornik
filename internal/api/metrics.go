package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// APIMetrics holds Prometheus metrics for the HTTP API.
type APIMetrics struct {
	RequestsTotal   *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	ActiveRequests  prometheus.Gauge

	// CostAttributionTotal counts cost-row writes labelled by
	// the AttributionSource that produced the project_id.
	// Operators dashboard this to see what fraction of their
	// billing rows come from the trustworthy key-bound path vs
	// the legacy header / fallback / anonymous paths — drives
	// the per-project-API-key migration backlog.
	CostAttributionTotal *prometheus.CounterVec

	// ExecutionsStuck is the current count of executions detected
	// stuck in RUNNING/PENDING past the watchdog threshold, labelled
	// by status. A gauge, not a counter: the doctor stuck-execution
	// scan is a point-in-time snapshot (the outcome-quality LLD's
	// watchdog), so each run SETs the count rather than accumulating.
	// The LLD names it vornik_executions_stuck (reconciled from the
	// earlier _total draft, which mis-suffixed a gauge — audit R13).
	ExecutionsStuck *prometheus.GaugeVec

	// ApprovalsTotal counts autonomy manual-approval resolutions,
	// labelled by project and decision ("approved" / "rejected").
	// vornik_autonomy_approvals_total — autonomy-approval-surface LLD §7
	// (was an optional/deferred metric; the approval surface shipped so
	// the counter is now registered at the resolve seam).
	ApprovalsTotal *prometheus.CounterVec

	// WebhookRelayReceivedTotal counts webhook deliveries received at the
	// job-tier mTLS relay ingress (/internal/v1/webhook-relay). The
	// always-on "the DMZ relay is forwarding to this worker" signal that was
	// missing — cluster-diagnostics LLD §3.3. vornik_webhook_relay_received_total.
	WebhookRelayReceivedTotal prometheus.Counter

	// NodeHeartbeatReceivedTotal counts node-heartbeat upserts received at
	// the relay ingress (/internal/v1/node-heartbeat), labelled by the
	// reporting node's profile. The always-on "DMZ nodes are heartbeating"
	// signal. vornik_node_heartbeat_received_total{profile}.
	NodeHeartbeatReceivedTotal *prometheus.CounterVec

	// SupportReportGeneratedTotal counts support-report bundles the
	// daemon produced, labelled by mode ("task"/"window") and raw
	// ("true"/"false"). vornik_support_report_generated_total —
	// support-report-design.md §10.
	SupportReportGeneratedTotal *prometheus.CounterVec

	// SupportReportBytesTotal accumulates the daemon-side bundle size
	// (bytes streamed back), labelled by mode.
	// vornik_support_report_bytes_total.
	SupportReportBytesTotal *prometheus.CounterVec
}

// NewAPIMetrics creates and registers API metrics.
func NewAPIMetrics(reg *prometheus.Registry) *APIMetrics {
	m := &APIMetrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "api",
			Name:      "requests_total",
			Help:      "Total HTTP API requests.",
		}, []string{"method", "path", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vornik",
			Subsystem: "api",
			Name:      "request_duration_seconds",
			Help:      "HTTP API request duration.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "path"}),
		ActiveRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "api",
			Name:      "active_requests",
			Help:      "Currently in-flight API requests.",
		}),
		CostAttributionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "api",
			Name:      "cost_attribution_total",
			Help:      "External-API cost rows grouped by attribution source. source labels: key-bound (DB-backed key, trustworthy), header (legacy X-Vornik-Project-ID — client-supplied, not trustworthy), fallback (daemon-wide pin), anonymous (_external sentinel). The key-bound fraction is the migration KPI — drive it to 100% to retire the legacy paths.",
		}, []string{"source"}),
		ExecutionsStuck: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vornik",
			Subsystem: "",
			Name:      "executions_stuck",
			Help:      "Executions detected stuck in RUNNING/PENDING past the watchdog threshold, by status. Set on each doctor stuck-execution scan.",
		}, []string{"status"}),
		ApprovalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Subsystem: "autonomy",
			Name:      "approvals_total",
			Help:      "Autonomy manual-approval resolutions, by project and decision (approved/rejected).",
		}, []string{"project", "decision"}),
		WebhookRelayReceivedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "webhook_relay_received_total",
			Help:      "Webhook deliveries received at the job-tier mTLS relay ingress (the DMZ relay forwarding to this worker).",
		}),
		NodeHeartbeatReceivedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "node_heartbeat_received_total",
			Help:      "Node-heartbeat upserts received at the relay ingress, by reporting node profile.",
		}, []string{"profile"}),
		SupportReportGeneratedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "support_report_generated_total",
			Help:      "Support-report bundles generated by the daemon, by mode (task/window) and raw (true/false).",
		}, []string{"mode", "raw"}),
		SupportReportBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "support_report_bytes_total",
			Help:      "Total bytes of support-report bundles streamed by the daemon, by mode.",
		}, []string{"mode"}),
	}
	reg.MustRegister(m.RequestsTotal, m.RequestDuration, m.ActiveRequests, m.CostAttributionTotal, m.ExecutionsStuck, m.ApprovalsTotal, m.WebhookRelayReceivedTotal, m.NodeHeartbeatReceivedTotal, m.SupportReportGeneratedTotal, m.SupportReportBytesTotal)
	return m
}

// RecordWebhookRelayReceived bumps the relay-ingress receipt counter. Nil-safe
// (the metrics registry is optional; handlers still work without it).
func (m *APIMetrics) RecordWebhookRelayReceived() {
	if m == nil || m.WebhookRelayReceivedTotal == nil {
		return
	}
	m.WebhookRelayReceivedTotal.Inc()
}

// RecordNodeHeartbeatReceived bumps the heartbeat-receipt counter for a
// reporting node's profile. Nil-safe.
func (m *APIMetrics) RecordNodeHeartbeatReceived(profile string) {
	if m == nil || m.NodeHeartbeatReceivedTotal == nil {
		return
	}
	if profile == "" {
		profile = "unknown"
	}
	m.NodeHeartbeatReceivedTotal.WithLabelValues(profile).Inc()
}

// SetExecutionsStuck records the current stuck-execution count per
// status from one watchdog scan. The map carries every status the scan
// observed so a status that dropped to zero is reset (the caller passes
// 0 for previously-seen statuses with no current stuck rows). Nil-safe.
func (m *APIMetrics) SetExecutionsStuck(byStatus map[string]int) {
	if m == nil || m.ExecutionsStuck == nil {
		return
	}
	for status, n := range byStatus {
		m.ExecutionsStuck.WithLabelValues(status).Set(float64(n))
	}
}

// RecordCostAttribution bumps the per-source counter on every
// cost-row write the chat-proxy + audit paths emit. Nil-safe
// (the wiring is optional; tests without a metrics registry
// still record rows but skip the counter).
func (m *APIMetrics) RecordCostAttribution(source AttributionSource) {
	if m == nil {
		return
	}
	m.CostAttributionTotal.WithLabelValues(string(source)).Inc()
}

// Middleware returns an HTTP middleware that records request metrics.
func (m *APIMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.ActiveRequests.Inc()

		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)

		m.ActiveRequests.Dec()
		path := normalizePath(r.URL.Path)
		status := strconv.Itoa(sw.status)
		m.RequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		m.RequestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer if it supports the
// interface. Required for SSE log streaming and the
// /ui/tasks/<id>/logs/stream endpoint to flush each chunk
// instead of buffering.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer so connection-upgrade
// paths (WebSocket on /api/v1/executions/{id}/live) can take
// over the underlying TCP connection. Without this, the metrics
// middleware silently breaks every WebSocket handler in the
// router — discovered 2026-05-23 when /api/v1/executions/
// {id}/live started returning 501 "Not Implemented" after the
// livePub wiring fix made the route reachable.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errStatusWriterNoHijack
}

var errStatusWriterNoHijack = errors.New("api metrics: wrapped ResponseWriter does not support Hijack")

// normalizePath collapses dynamic path segments to reduce label cardinality.
// Known patterns are replaced by canonical forms; all others are returned as-is
// (truncated to 80 chars as a safety net).
//
// Examples:
//
//	/api/v1/projects/my-project/tasks/task-abc123            → /api/v1/projects/{id}/tasks/{id}
//	/api/v1/projects/my-project/tasks/task-abc123/cancel     → /api/v1/projects/{id}/tasks/{id}/cancel
//	/api/v1/projects/my-project/tasks                        → /api/v1/projects/{id}/tasks
//	/api/v1/projects/my-project/executions                   → /api/v1/projects/{id}/executions
//	/api/v1/executions/exec-abc123                           → /api/v1/executions/{id}
//	/api/v1/executions/exec-abc123/pause                     → /api/v1/executions/{id}/pause
func normalizePath(path string) string {
	// Strip trailing slash for matching
	p := strings.TrimRight(path, "/")

	// /api/v1/projects/{id}/...
	if after, ok := stripPrefix(p, "/api/v1/projects/"); ok {
		// Find next segment boundary
		slash := strings.Index(after, "/")
		if slash < 0 {
			return "/api/v1/projects/{id}"
		}
		rest := after[slash:]
		// /api/v1/projects/{id}/tasks/{id}/action
		if after2, ok2 := stripPrefix(rest, "/tasks/"); ok2 {
			slash2 := strings.Index(after2, "/")
			if slash2 < 0 {
				return "/api/v1/projects/{id}/tasks/{id}"
			}
			return "/api/v1/projects/{id}/tasks/{id}" + after2[slash2:]
		}
		// /api/v1/projects/{id}/tasks or /api/v1/projects/{id}/executions
		return "/api/v1/projects/{id}" + rest
	}

	// /api/v1/executions/{id}/...
	if after, ok := stripPrefix(p, "/api/v1/executions/"); ok {
		slash := strings.Index(after, "/")
		if slash < 0 {
			return "/api/v1/executions/{id}"
		}
		return "/api/v1/executions/{id}" + after[slash:]
	}

	if len(p) > 80 {
		return p[:80]
	}
	return p
}

func stripPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}
