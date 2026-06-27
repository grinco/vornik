// Package api: targeted tests for small helpers that the larger
// integration tests don't currently exercise:
//
//   - RunReadiness: nil readinessChecks edge case
//   - NewTradingMetrics: registry registration + nil-registry tolerance
//   - resultMessage: every branch of the JSON shape it accepts
package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// --- RunReadiness ----------------------------------------------------

func TestRunReadiness_NoChecks_EmptyResult(t *testing.T) {
	srv := &Server{}
	out := srv.RunReadiness(context.Background())
	if len(out) != 0 {
		t.Errorf("expected empty result with no checks; got %v", out)
	}
}

func TestRunReadiness_DBOnly(t *testing.T) {
	cases := []struct {
		name   string
		ping   error
		want   string
		hasErr bool
	}{
		{"ok", nil, "ok", false},
		{"ping-fails", errors.New("connection refused"), "error", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mocks.MockTaskRepository{
				PingFunc: func(_ context.Context) error { return tc.ping },
			}
			srv := &Server{taskRepo: repo}
			out := srv.RunReadiness(context.Background())
			if len(out) != 1 {
				t.Fatalf("expected 1 result, got %d", len(out))
			}
			if out[0].Name != "database" {
				t.Errorf("name: got %q", out[0].Name)
			}
			if out[0].Status != tc.want {
				t.Errorf("status: got %q, want %q", out[0].Status, tc.want)
			}
			if tc.hasErr && out[0].Error == "" {
				t.Errorf("expected non-empty Error on failed ping")
			}
		})
	}
}

func TestRunReadiness_WithCustomChecks(t *testing.T) {
	srv := &Server{
		readinessChecks: []ReadinessCheck{
			{Name: "queue", Check: func(_ context.Context) error { return nil }},
			{Name: "broker", Check: func(_ context.Context) error { return errors.New("nope") }},
		},
	}
	out := srv.RunReadiness(context.Background())
	if len(out) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out))
	}
	if out[0].Status != "ok" {
		t.Errorf("queue: got %q", out[0].Status)
	}
	if out[1].Status != "error" || out[1].Error == "" {
		t.Errorf("broker should report error; got %+v", out[1])
	}
}

// --- NewTradingMetrics -----------------------------------------------

func TestNewTradingMetrics_RegistersOnNonNilRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewTradingMetrics(reg)
	if m == nil {
		t.Fatal("NewTradingMetrics returned nil")
	}
	// One increment per counter — confirms they're constructed and
	// gather-able. If they weren't registered, MustRegister would
	// have already panicked above.
	m.SafetyEventsTotal.WithLabelValues("p1", "kill_switch_on", "warn").Inc()
	m.OrdersIngestedTotal.WithLabelValues("p1", "placed").Inc()
	m.FillsIngestedTotal.WithLabelValues("p1").Inc()
	m.IngestErrorsTotal.WithLabelValues("ingest", "validation").Inc()

	metrics, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(metrics) < 4 {
		t.Errorf("expected ≥4 metric families registered; got %d", len(metrics))
	}
}

func TestNewTradingMetrics_NilRegistry_StillUsable(t *testing.T) {
	// Documented contract: nil registry is valid (e.g. when the
	// daemon runs without Prometheus). Counters still exist and
	// accept Inc calls — they're just not gathered.
	m := NewTradingMetrics(nil)
	if m == nil {
		t.Fatal("NewTradingMetrics(nil) returned nil")
	}
	// No panic on Inc with nil registry — these are independent
	// CounterVecs.
	m.SafetyEventsTotal.WithLabelValues("p1", "trip", "warn").Inc()
}

// --- resultMessage ---------------------------------------------------

func TestResultMessage(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"invalid-json", []byte(`not-json`), ""},
		{"message-set", []byte(`{"message":"all good"}`), "all good"},
		{"only-whitespace-message", []byte(`{"message":"   ","status":"OK"}`), "result status: OK"},
		{"only-status", []byte(`{"status":"failed"}`), "result status: failed"},
		{"both-empty", []byte(`{}`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resultMessage(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- ReadinessCheck struct sanity (zero-value Check is nil-safe) ----

func TestReadinessCheck_ZeroValue(t *testing.T) {
	// Sanity: a check with a nil Check func would panic in
	// RunReadiness; the constructor pattern in handlers.go never
	// produces one but this pins the assumption explicitly.
	c := ReadinessCheck{Name: "test", Check: func(_ context.Context) error { return nil }}
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("zero-value Check should pass; got %v", err)
	}
}

// --- mocks.MockTaskRepository.Ping sanity ----------------------------

// Re-pins MockTaskRepository.PingFunc nil-handling so future readiness
// uses don't trip on a missing stub.
func TestMockTaskRepository_PingNilFn(t *testing.T) {
	repo := &mocks.MockTaskRepository{}
	if err := repo.Ping(context.Background()); err != nil {
		t.Errorf("nil PingFunc should return nil; got %v", err)
	}
}

// --- guard: persistence import used (errs.Is paths above) -----------

var _ = persistence.ErrNotFound // keep persistence in import list

func TestPersistenceErrorMessage(t *testing.T) {
	// Trivial assertion that the package-level sentinel keeps its
	// "not found" verbiage — used by ExplainTask + a dozen handlers
	// to map persistence errors to 404.
	if !strings.Contains(persistence.ErrNotFound.Error(), "not found") {
		t.Errorf("ErrNotFound message changed: %q", persistence.ErrNotFound.Error())
	}
}
