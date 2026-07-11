package ui

// Guided Integrations Hub metrics (task 5.4, design §5.8). Registered on
// the SAME registry the API metrics use (internal/api/metrics.go's
// NewAPIMetrics idiom: struct of *prometheus.CounterVec + a constructor
// that MustRegisters them + nil-safe Record* methods) — every handler call
// site stays correct whether or not a registry was ever wired (most
// tests construct a bare NewServer() with no WithIntegrationsMetrics
// option).
//
// The constructor is called by the CONTAINER, not by a ServerOption here
// (unlike a plain registry-in/metrics-out option) — mirroring
// api.WithRateLimitMetrics / api.WithDryRunMetrics / api.WithChainMetrics,
// which all take a pre-built *T rather than a *prometheus.Registry. That
// indirection exists for a documented reason (container_http.go's
// "TWO-PASS TRAP" / "INVISIBLE METRIC" incident, 2026-06-06):
// initHTTPServer runs twice (once before observability exists, once
// after), and building+registering a fresh metrics struct on EVERY call
// would MustRegister the same collector names a second time on the same
// registry and panic. The container guards construction with an
// "if c.integrationsMetrics == nil" check (exactly like dryRunMetrics)
// and passes the already-built pointer in — so this file must NOT expose
// a registry-taking ServerOption, only WithIntegrationsMetrics.

import "github.com/prometheus/client_golang/prometheus"

// IntegrationsMetrics holds the Guided Integrations Hub's Prometheus
// counters.
type IntegrationsMetrics struct {
	// ProbeTotal counts every Prober.Probe call the hub makes — the
	// direct "Test connection" handler, the save handler's built-in
	// re-probe (design §5.4 step 1), and the explicit re-check action —
	// labelled by kind and outcome (ok|fail|error).
	ProbeTotal *prometheus.CounterVec
	// SaveTotal counts each save handler's terminal outcome, labelled by
	// kind and result (ok|probe_failed|write_failed|reload_failed).
	SaveTotal *prometheus.CounterVec
}

// NewIntegrationsMetrics creates and registers the Integrations Hub
// metrics on reg.
func NewIntegrationsMetrics(reg *prometheus.Registry) *IntegrationsMetrics {
	m := &IntegrationsMetrics{
		ProbeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "integration_probe_total",
			Help:      "Guided Integrations Hub probe attempts, by kind and outcome (ok/fail/error).",
		}, []string{"kind", "outcome"}),
		SaveTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "integration_save_total",
			Help:      "Guided Integrations Hub save attempts, by kind and terminal result (ok/probe_failed/write_failed/reload_failed).",
		}, []string{"kind", "result"}),
	}
	reg.MustRegister(m.ProbeTotal, m.SaveTotal)
	return m
}

// RecordProbe bumps the per-(kind,outcome) probe counter. outcome is the
// string form of integrations.Outcome (ok|fail|error) — a plain string
// rather than the typed Outcome to keep this file free of the
// internal/integrations import. Nil-safe (the registry wiring is
// optional) and a no-op for an empty outcome (a probe that never actually
// ran — e.g. Save short-circuited before step 1).
func (m *IntegrationsMetrics) RecordProbe(kind, outcome string) {
	if m == nil || m.ProbeTotal == nil || outcome == "" {
		return
	}
	m.ProbeTotal.WithLabelValues(kind, outcome).Inc()
}

// RecordSave bumps the per-(kind,result) save counter. Nil-safe.
func (m *IntegrationsMetrics) RecordSave(kind, result string) {
	if m == nil || m.SaveTotal == nil {
		return
	}
	m.SaveTotal.WithLabelValues(kind, result).Inc()
}

// WithIntegrationsMetrics wires an already-constructed IntegrationsMetrics
// onto the Server. A nil m is a harmless no-op — every Record* call is
// nil-safe, so the hub's handlers behave exactly as if this option had
// never been passed.
func WithIntegrationsMetrics(m *IntegrationsMetrics) ServerOption {
	return func(s *Server) {
		s.integrationsMetrics = m
	}
}
