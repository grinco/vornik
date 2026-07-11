package narrator

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestRun_StructurallyDisabled_MissingWiring — Run must return
// immediately (never subscribe, never panic) when any required
// collaborator is nil, matching LLMConsolidateWorker.Run's contract.
func TestRun_StructurallyDisabled_MissingWiring(t *testing.T) {
	cases := []struct {
		name string
		n    *Narrator
	}{
		{"nil receiver", nil},
		{"nil Sub", &Narrator{Pub: newFakePub(nil), Store: newFakeStore(nil), Executions: newFakeExecutions()}},
		{"nil Pub", &Narrator{Sub: newFakeSub(), Store: newFakeStore(nil), Executions: newFakeExecutions()}},
		{"nil Store", &Narrator{Sub: newFakeSub(), Pub: newFakePub(nil), Executions: newFakeExecutions()}},
		{"nil Executions", &Narrator{Sub: newFakeSub(), Pub: newFakePub(nil), Store: newFakeStore(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				tc.n.Run(context.Background())
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run should return immediately when structurally disabled")
			}
		})
	}
}

// TestRun_PanicRecovery_ResubscribesAndCountsMetric — design §5.1: a
// panic anywhere in the event-processing path is recovered, counted
// (vornik_narration_panics_total), and the loop re-subscribes so a
// single bad event never takes narration down for the rest of the
// daemon's life.
func TestRun_PanicRecovery_ResubscribesAndCountsMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := true
	h := newTestHarness(t, func(n *Narrator) {
		n.Metrics = NewMetrics(reg)
		n.preLine = func(_ *persistence.ExecutionNarration) {
			if first {
				first = false
				panic("simulated onLine panic")
			}
		}
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "worker"})

	// The panicking call never reaches h.Lines (onLine panicked
	// before it could send), so wait on the metric instead of
	// awaitLine.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if counterValue(t, h.N.Metrics.PanicsTotal) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := counterValue(t, h.N.Metrics.PanicsTotal); got < 1 {
		t.Fatalf("panics_total = %v, want >= 1", got)
	}

	// The s1 line's own persist happened BEFORE preLine panicked
	// (emitLine persists, then publishes, then calls preLine/onLine —
	// see narrator.go), so it landed in h.Store despite never reaching
	// h.Lines. h.Store is the fake Store, entirely separate from
	// n.states (the in-memory per-execution state the panic recovery
	// resets) — its seqByExec map survives the recovery exactly like
	// the real DB's seq (computed via MAX(seq)+1 per execution_id)
	// would survive a daemon-side recover(). Capture it now, before
	// the resubscribe, so the before/after comparison below is
	// meaningful rather than trivially true.
	preRows := h.Store.all()
	if len(preRows) != 1 {
		t.Fatalf("expected exactly 1 persisted row before the resubscribe (the panicked s1 line), got %d", len(preRows))
	}
	preSeq := preRows[0].Seq

	// Resubscribe worked: a fresh event on the SAME execution_id
	// produces a line (state was reset by the recovery, but a new
	// StepStarted still resolves + narrates normally).
	seedRunningExecution(h)
	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s2", Role: "worker"})
	row := h.awaitLine(3 * time.Second)
	if row == nil {
		t.Fatal("expected a line after the panic-recovery resubscribe")
	}

	// The line persisted AFTER the panic (this one) must carry a
	// strictly greater seq than the one persisted BEFORE it, for the
	// same execution_id — proving seq stays monotonically increasing
	// across a narrator panic/resubscribe, not reset to 0 alongside
	// n.states.
	if row.Seq <= preSeq {
		t.Fatalf("seq not monotonically increasing across panic recovery: pre-panic seq=%d, post-panic seq=%d", preSeq, row.Seq)
	}
}
