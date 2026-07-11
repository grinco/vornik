package service

// Regression tests for the deliverable-first completion "send to chat"
// metric wiring in initHTTPServer (task 2.4, narrated-execution-design.md
// §5.8/§5.9). Mirrors container_http_integrations_metrics_test.go
// byte-for-byte in shape — c.deliverableMetrics follows the EXACT same
// "TWO-PASS TRAP" / "INVISIBLE METRIC" contract: initHTTPServer runs
// twice (pass 1 inside NewContainer, no observability yet; pass 2 from
// NewContainerWithObservability once the served registry exists), so the
// metrics struct must be built ONLY on pass 2 and guarded so a
// hypothetical third pass never re-registers the same collector name on
// the same registry (which would panic).

import (
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/ui"
)

// buildDeliverableMetricsLikeInitHTTPServer mirrors the production wiring
// block in initHTTPServer byte-for-byte in shape: build only inside the
// reg != nil branch, guarded once-only. Keep in sync with
// container_http.go — the comment there points here.
func buildDeliverableMetricsLikeInitHTTPServer(c *Container) {
	if reg := c.observabilityRegistry(); reg != nil {
		if c.deliverableMetrics == nil {
			c.deliverableMetrics = ui.NewDeliverableMetrics(reg)
		}
	}
}

// TestDeliverableMetrics_TwoPassWiring drives the production wiring shape
// through both initHTTPServer passes: pass 1 (no observability) must leave
// deliverableMetrics nil, and pass 2 must build it on the custom registry
// that /metrics serves.
func TestDeliverableMetrics_TwoPassWiring(t *testing.T) {
	c := &Container{} // pass 1: Observability nil

	buildDeliverableMetricsLikeInitHTTPServer(c)
	if c.deliverableMetrics != nil {
		t.Fatal("pass 1 (no observability) must leave deliverableMetrics nil")
	}

	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c.Observability = obs
	buildDeliverableMetricsLikeInitHTTPServer(c)
	if c.deliverableMetrics == nil {
		t.Fatal("pass 2 (observability attached) must build deliverableMetrics")
	}

	c.deliverableMetrics.RecordSend("email")
	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatalf("custom registry Gather() error: %v", err)
	}
	const wantName = "vornik_deliverable_sends_total"
	for _, mf := range mfs {
		if mf.GetName() == wantName {
			return // found on the served registry — regression absent
		}
	}
	t.Fatalf("%q not found in the custom observability registry — counter registered on the wrong registerer", wantName)
}

// TestDeliverableMetrics_SecondPassIsOnceOnly: a third initHTTPServer-style
// pass must not re-register on the same registry — MustRegister would
// panic on a duplicate.
func TestDeliverableMetrics_SecondPassIsOnceOnly(t *testing.T) {
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c := &Container{Observability: obs}
	buildDeliverableMetricsLikeInitHTTPServer(c)
	first := c.deliverableMetrics
	buildDeliverableMetricsLikeInitHTTPServer(c) // must not panic, must not replace
	if c.deliverableMetrics != first {
		t.Fatal("repeat pass replaced the deliverableMetrics instance")
	}
}
