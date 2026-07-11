package ui

// Outcome Inbox metrics (task 4.4, design §5.8). Follows the exact idiom
// internal/ui/integrations_metrics.go established: a struct of
// *prometheus.CounterVec + a constructor that MustRegisters it + a
// nil-safe Record method, built by the CONTAINER (not a registry-taking
// ServerOption here) so the two-pass initHTTPServer re-entry never
// double-registers the same collector name (see integrations_metrics.go's
// doc comment on the 2026-06-06 "TWO-PASS TRAP" incident).

import "github.com/prometheus/client_golang/prometheus"

// InboxMetrics holds the Outcome Inbox's Prometheus counter.
type InboxMetrics struct {
	// ViewsTotal counts every Inbox() render, labelled by the viewer's
	// session role ("admin" / "user" / "none" for an unauthenticated or
	// auth-disabled request — see inboxMetricsRole in inbox.go).
	ViewsTotal *prometheus.CounterVec
}

// NewInboxMetrics creates and registers the inbox-views counter on reg.
func NewInboxMetrics(reg *prometheus.Registry) *InboxMetrics {
	m := &InboxMetrics{
		ViewsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "ui_inbox_views_total",
			Help:      "Outcome Inbox page renders, by viewer session role.",
		}, []string{"role"}),
	}
	reg.MustRegister(m.ViewsTotal)
	return m
}

// RecordView bumps the per-role view counter. Nil-safe — a Server built
// without WithInboxMetrics (most tests, and pass 1 of the two-pass HTTP
// init) just skips the increment.
func (m *InboxMetrics) RecordView(role string) {
	if m == nil || m.ViewsTotal == nil || role == "" {
		return
	}
	m.ViewsTotal.WithLabelValues(role).Inc()
}

// WithInboxMetrics wires an already-constructed InboxMetrics onto the
// Server. A nil m is a harmless no-op.
func WithInboxMetrics(m *InboxMetrics) ServerOption {
	return func(s *Server) { s.inboxMetrics = m }
}
