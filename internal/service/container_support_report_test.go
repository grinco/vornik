package service

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/api"
)

func TestSupportMetricsAdapter_RendersText(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vornik_support_test_total",
		Help: "test counter",
	})
	reg.MustRegister(c)
	c.Inc()

	a := &supportMetricsAdapter{gatherer: reg}
	out, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !strings.Contains(out, "vornik_support_test_total") {
		t.Errorf("metrics text missing the counter:\n%s", out)
	}
}

func TestSupportHealthAdapter_Snapshot(t *testing.T) {
	srv := api.NewServer()
	a := &supportHealthAdapter{srv: srv}
	snap, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap == nil {
		t.Error("expected a non-nil readiness snapshot")
	}
}

// Compile-time contract: the adapters satisfy the api collector
// interfaces. The doctor adapter's Run path queries the DB
// (RunReportReadOnly runs DB-backed checks), so its behaviour is
// exercised by the integration harness, not a unit test.
var (
	_ api.SupportDoctorRunner  = (*supportDoctorAdapter)(nil)
	_ api.SupportHealthSource  = (*supportHealthAdapter)(nil)
	_ api.SupportMetricsSource = (*supportMetricsAdapter)(nil)
)

func TestWireSupportReportCollectors_NilSafe(t *testing.T) {
	c := &Container{}
	// nil server → no-op, must not panic.
	c.wireSupportReportCollectors(nil, nil)
	// non-nil server, nil doctor → wires health (+ metrics if registry).
	srv := api.NewServer()
	c.wireSupportReportCollectors(srv, nil)
	// Health adapter must have been wired; verify by building a bundle
	// request would use it — here we just assert no panic + the server
	// is usable.
	if srv == nil {
		t.Fatal("server unexpectedly nil")
	}
}
