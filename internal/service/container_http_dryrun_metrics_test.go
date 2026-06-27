package service

// Regression tests for the dry-run metrics wiring in initHTTPServer.
//
// Incident chain (both fixed 2026-06-06):
//
//  1. STARTUP CRASH-LOOP: container_http.go passed
//     c.observabilityRegistry() — a possibly-nil *prometheus.Registry —
//     straight into NewDryRunMetrics' Registerer parameter. A typed nil
//     makes the interface non-nil, defeats the nil fallback, and promauto
//     SIGSEGVs (same class as the route-queue registry panic in the
//     ROADMAP). The daemon crash-looped on boot with metrics disabled.
//
//  2. INVISIBLE METRIC: initHTTPServer runs TWICE — pass 1 inside
//     NewContainer (no observability yet), pass 2 from
//     NewContainerWithObservability after the registry exists (the
//     deliberate "rebuild the HTTP server so /metrics uses the custom
//     registry" step). Building dryRunMetrics unconditionally on pass 1
//     registered the counter on prometheus.DefaultRegisterer; the
//     once-only guard then kept that instance on pass 2 while /metrics
//     switched to the custom registry — denials incremented invisibly
//     (observed live: auth-dryrun WARNs flowing, zero series at /metrics).
//
// Contract now: dryRunMetrics is built ONLY when the observability
// registry exists (mirroring rateLimitMetrics). Pass 1 leaves it nil —
// the middleware is nil-safe and the pass-1 server never serves requests.

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/observability"
)

// buildDryRunMetricsLikeInitHTTPServer mirrors the production wiring
// block in initHTTPServer byte-for-byte in shape: build only inside the
// reg != nil branch, guarded once-only. Keep in sync with
// container_http.go — the comment there points here.
func buildDryRunMetricsLikeInitHTTPServer(c *Container) {
	if reg := c.observabilityRegistry(); reg != nil {
		if c.dryRunMetrics == nil {
			c.dryRunMetrics = api.NewDryRunMetrics(reg)
		}
	}
}

// TestDryRunMetrics_TwoPassWiring drives the production wiring shape
// through both initHTTPServer passes: pass 1 (no observability) must
// leave dryRunMetrics nil — NOT register on DefaultRegisterer — and
// pass 2 must register on the custom registry that /metrics serves.
func TestDryRunMetrics_TwoPassWiring(t *testing.T) {
	c := &Container{} // pass 1: Observability nil

	buildDryRunMetricsLikeInitHTTPServer(c)
	if c.dryRunMetrics != nil {
		t.Fatal("pass 1 (no observability) must leave dryRunMetrics nil — a DefaultRegisterer counter becomes invisible once /metrics switches to the custom registry")
	}

	// Pass 2: observability attached (as NewContainerWithObservability does).
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c.Observability = obs
	buildDryRunMetricsLikeInitHTTPServer(c)
	if c.dryRunMetrics == nil {
		t.Fatal("pass 2 (observability attached) must build dryRunMetrics")
	}

	// The counter must be gatherable from the registry /metrics serves.
	c.dryRunMetrics.DenialsTotal.WithLabelValues("missing_credential").Inc()
	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatalf("custom registry Gather() error: %v", err)
	}
	const wantName = "vornik_auth_dryrun_denials_total"
	for _, mf := range mfs {
		if mf.GetName() == wantName {
			return // found on the served registry — regression absent
		}
	}
	t.Fatalf("%q not found in the custom observability registry — counter registered on the wrong registerer", wantName)
}

// TestDryRunMetrics_SecondPassIsOnceOnly: a third initHTTPServer-style
// pass (e.g. future re-init) must not re-register on the same registry —
// promauto.MustRegister would panic on a duplicate.
func TestDryRunMetrics_SecondPassIsOnceOnly(t *testing.T) {
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c := &Container{Observability: obs}
	buildDryRunMetricsLikeInitHTTPServer(c)
	first := c.dryRunMetrics
	buildDryRunMetricsLikeInitHTTPServer(c) // must not panic, must not replace
	if c.dryRunMetrics != first {
		t.Fatal("repeat pass replaced the dryRunMetrics instance")
	}
}

// TestDryRunMetrics_NilObservability_RegistryIsNil pins the concrete-nil
// contract the wiring relies on: an un-initialised container yields a nil
// *prometheus.Registry (NOT a typed-nil interface downstream — the
// reg != nil guard in the wiring is the typed-nil firewall).
func TestDryRunMetrics_NilObservability_RegistryIsNil(t *testing.T) {
	c := &Container{}
	if got := c.observabilityRegistry(); got != nil {
		t.Fatalf("observabilityRegistry() = %v, want nil for un-initialised container", got)
	}
}
