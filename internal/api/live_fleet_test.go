package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fleetFakeSub feeds a caller-controlled channel to SubscribeAll.
type fleetFakeSub struct{ ch chan livepubsub.LiveEvent }

func (f *fleetFakeSub) Subscribe(string, int64) (<-chan livepubsub.LiveEvent, func(), error) {
	return f.ch, func() {}, nil
}
func (f *fleetFakeSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return f.ch, func() {}, nil
}
func (f *fleetFakeSub) Publish(context.Context, string, string, any) int64 { return 0 }

// TestFleetLive_FiltersSummaryKindsAndScope — the fleet SSE forwards only
// summary kinds for executions the caller may see, as `fleet-changed` events.
func TestFleetLive_FiltersSummaryKindsAndScope(t *testing.T) {
	sub := &fleetFakeSub{ch: make(chan livepubsub.LiveEvent)}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if id == "e-ok" {
				return &persistence.Execution{ID: "e-ok", ProjectID: "p1"}, nil
			}
			return nil, errors.New("not found") // e-deny → unresolvable → scoped out
		},
	}
	srv := NewServer(WithLiveSubscriber(sub), WithExecutionRepository(execRepo))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/live/fleet", nil)

	done := make(chan struct{})
	go func() { srv.FleetLive(rec, req); close(done) }()

	// Ordered, unbuffered → handler processes each before the next send.
	sub.ch <- livepubsub.LiveEvent{ExecutionID: "e-ok", Kind: "step_started"}     // forwarded
	sub.ch <- livepubsub.LiveEvent{ExecutionID: "e-ok", Kind: "llm_token"}        // non-summary → dropped
	sub.ch <- livepubsub.LiveEvent{ExecutionID: "e-deny", Kind: "step_completed"} // scoped out
	close(sub.ch)                                                                 // ends the handler loop

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FleetLive did not return after channel close")
	}

	body := rec.Body.String()
	if n := strings.Count(body, "event: fleet-changed"); n != 1 {
		t.Fatalf("fleet-changed count = %d, want 1\nbody:\n%s", n, body)
	}
	if !strings.Contains(body, `"execution_id":"e-ok"`) {
		t.Errorf("missing the allowed exec's frame:\n%s", body)
	}
	if strings.Contains(body, "e-deny") {
		t.Errorf("scoped-out exec leaked into the feed:\n%s", body)
	}
}
