package service

// Regression tests for the Guided Integrations Hub metrics wiring in
// initHTTPServer (task 5.4). Mirrors container_http_dryrun_metrics_test.go
// byte-for-byte in shape — c.integrationsMetrics follows the EXACT same
// "TWO-PASS TRAP" / "INVISIBLE METRIC" contract documented there:
// initHTTPServer runs twice (pass 1 inside NewContainer, no observability
// yet; pass 2 from NewContainerWithObservability once the served registry
// exists), so the metrics struct must be built ONLY on pass 2 and guarded
// so a hypothetical third pass never re-registers the same collector
// names on the same registry (which would panic).

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/ui"
)

// buildIntegrationsMetricsLikeInitHTTPServer mirrors the production wiring
// block in initHTTPServer byte-for-byte in shape: build only inside the
// reg != nil branch, guarded once-only. Keep in sync with
// container_http.go — the comment there points here.
func buildIntegrationsMetricsLikeInitHTTPServer(c *Container) {
	if reg := c.observabilityRegistry(); reg != nil {
		if c.integrationsMetrics == nil {
			c.integrationsMetrics = ui.NewIntegrationsMetrics(reg)
		}
	}
}

// TestIntegrationsMetrics_TwoPassWiring drives the production wiring shape
// through both initHTTPServer passes: pass 1 (no observability) must leave
// integrationsMetrics nil, and pass 2 must build it on the custom registry
// that /metrics serves.
func TestIntegrationsMetrics_TwoPassWiring(t *testing.T) {
	c := &Container{} // pass 1: Observability nil

	buildIntegrationsMetricsLikeInitHTTPServer(c)
	if c.integrationsMetrics != nil {
		t.Fatal("pass 1 (no observability) must leave integrationsMetrics nil")
	}

	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c.Observability = obs
	buildIntegrationsMetricsLikeInitHTTPServer(c)
	if c.integrationsMetrics == nil {
		t.Fatal("pass 2 (observability attached) must build integrationsMetrics")
	}

	c.integrationsMetrics.RecordProbe("telegram", "ok")
	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatalf("custom registry Gather() error: %v", err)
	}
	const wantName = "vornik_integration_probe_total"
	for _, mf := range mfs {
		if mf.GetName() == wantName {
			return // found on the served registry — regression absent
		}
	}
	t.Fatalf("%q not found in the custom observability registry — counter registered on the wrong registerer", wantName)
}

// TestIntegrationsMetrics_SecondPassIsOnceOnly: a third initHTTPServer-style
// pass must not re-register on the same registry — MustRegister would
// panic on a duplicate.
func TestIntegrationsMetrics_SecondPassIsOnceOnly(t *testing.T) {
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c := &Container{Observability: obs}
	buildIntegrationsMetricsLikeInitHTTPServer(c)
	first := c.integrationsMetrics
	buildIntegrationsMetricsLikeInitHTTPServer(c) // must not panic, must not replace
	if c.integrationsMetrics != first {
		t.Fatal("repeat pass replaced the integrationsMetrics instance")
	}
}
