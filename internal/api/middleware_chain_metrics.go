package api

// Auth-chain backend-verdict metric. Added on the 2026-06-07
// architecture review's suggestion 8: internal/auth carried zero
// metrics, so after the 2026-06-06 auth flip there was no way to see
// WHICH backend admits or denies traffic — or to notice a backend
// silently going quiet (e.g. the session backend after a schema
// problem) — without grepping logs.
//
// Same lifecycle contract as DryRunMetrics: registered once per
// process (or per test registry), nil-safe at the record site, and
// the SAME instance must be passed to both the api router and the UI
// subtree (a CounterVec registers once per Registerer).

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// AuthChainMetrics holds the backend-verdict counter for the
// pluggable auth chain.
type AuthChainMetrics struct {
	VerdictsTotal *prometheus.CounterVec
}

// NewAuthChainMetrics registers and returns the chain-verdict counter.
// Pass nil to use prometheus.DefaultRegisterer. Call from the
// container's pass-2 HTTP build exactly like NewDryRunMetrics — never
// on pass 1 (see the TYPED-NIL TRAP note in container_http.go).
func NewAuthChainMetrics(registerer prometheus.Registerer) *AuthChainMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &AuthChainMetrics{
		VerdictsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "auth_backend_verdicts_total",
				Help: "Auth-chain resolutions by admitting backend and verdict. " +
					"backend = the chain entry that admitted (session | hmac-webhook | db-keys | static-keys), " +
					"or \"none\" when every backend declined; verdict = admitted | denied.",
			},
			[]string{"backend", "verdict"},
		),
	}
}

// recordChainVerdict increments the verdict counter. Nil-safe on both
// the receiver and the embedded CounterVec so the middleware can call
// it unconditionally.
func (m *AuthChainMetrics) recordChainVerdict(backend, verdict string) {
	if m == nil || m.VerdictsTotal == nil {
		return
	}
	m.VerdictsTotal.WithLabelValues(backend, verdict).Inc()
}
