package realip

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the operator-visible observability surface for real-IP
// resolution. The single counter lights up a spoof-attempt panel: a
// forwarding header arriving from a source that is NOT in trusted_proxies
// is either a misconfiguration (the proxy isn't actually listed) or a
// deliberate spoof attempt. Either way the operator should see it.
//
// Importing prometheus here does NOT violate the package's leaf
// constraint: the rule is "no internal/api, internal/ui or internal/config
// import" (cycle-freedom); prometheus is an external dependency every
// sibling Metrics type (ratelimit, chat, dispatcher, …) already uses. The
// middleware itself stays metric-agnostic via the onUntrustedHeader
// callback, so the resolver/middleware remain stdlib-only.
type Metrics struct {
	// UntrustedHeaderTotal counts requests that carried a client-IP
	// forwarding header (the configured header, X-Forwarded-For, or
	// CF-Connecting-IP) from a source NOT in trusted_proxies. Such headers
	// are ignored by the resolver; the counter exposes the spoof-attempt /
	// misconfiguration rate.
	UntrustedHeaderTotal prometheus.Counter
}

// NewMetrics registers the real-IP collectors on registerer (defaults to
// prometheus.DefaultRegisterer when nil), consistent with sibling
// *.NewMetrics constructors.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		UntrustedHeaderTotal: promauto.With(registerer).NewCounter(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "realip",
				Name:      "untrusted_header_total",
				Help:      "Requests carrying a client-IP forwarding header (CF-Connecting-IP / X-Forwarded-For) from a source not in trusted_proxies. The header is ignored; this is the spoof-attempt / proxy-misconfiguration rate.",
			},
		),
	}
}
