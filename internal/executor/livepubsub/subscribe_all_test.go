package livepubsub

import (
	"context"
	"testing"
	"time"
)

// TestSubscribeAll_ReceivesEventsAcrossExecutions — the fleet tap sees events
// from EVERY execution (live), not just one, and the per-execution Subscribe
// path is unaffected.
func TestSubscribeAll_ReceivesEventsAcrossExecutions(t *testing.T) {
	p := New(0)
	ch, cancel, err := p.SubscribeAll()
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	defer cancel()

	p.Publish(context.Background(), "exec-A", "step_started", nil)
	p.Publish(context.Background(), "exec-B", "step_completed", nil)

	got := map[string]bool{}
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case e := <-ch:
			got[e.ExecutionID] = true
		case <-deadline:
			t.Fatalf("timed out; received %v, want exec-A + exec-B", got)
		}
	}
	if !got["exec-A"] || !got["exec-B"] {
		t.Errorf("fleet tap missed an execution: %v", got)
	}
}

// TestSubscribeAll_CancelStopsDelivery — after cancel the fleet subscriber is
// removed and a later publish doesn't block (drop-on-full is non-blocking
// regardless, but the subscriber must be unregistered).
func TestSubscribeAll_CancelStopsDelivery(t *testing.T) {
	p := New(0).(*inProcessPublisher)
	_, cancel, _ := p.SubscribeAll()

	if n := len(p.allSubs); n != 1 {
		t.Fatalf("allSubs = %d, want 1 after SubscribeAll", n)
	}
	cancel()
	if n := len(p.allSubs); n != 0 {
		t.Errorf("allSubs = %d, want 0 after cancel", n)
	}
	cancel() // idempotent
}

// TestSubscribeAll_IngestRemoteReachesFleetTap — cross-replica events (fed via
// IngestRemote, as the DB-backed wrapper does) also reach the fleet tap, so a
// single SubscribeAll covers all replicas.
func TestSubscribeAll_IngestRemoteReachesFleetTap(t *testing.T) {
	p := New(0).(*inProcessPublisher)
	ch, cancel, _ := p.SubscribeAll()
	defer cancel()

	p.IngestRemote(LiveEvent{ExecutionID: "exec-remote", Seq: 7, Kind: "step_completed", Timestamp: time.Now().UTC()})

	select {
	case e := <-ch:
		if e.ExecutionID != "exec-remote" {
			t.Errorf("got %q, want exec-remote", e.ExecutionID)
		}
	case <-time.After(time.Second):
		t.Fatal("fleet tap did not receive the cross-replica (IngestRemote) event")
	}
}
