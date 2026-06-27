// Coverage for priority_queue.do()'s context-cancellation branch.
// The existing tests in priority_queue_test.go cover the normal
// happy-path ordering — this fills in the "caller cancels before
// the worker picks up the call" path.

package chat

import (
	"context"
	"testing"
	"time"
)

func TestQueuedProvider_ContextCancelledBeforeRun(t *testing.T) {
	// One worker, blocked by the first call's release channel; a
	// second call sits in the queue and we cancel its context
	// before the worker can pick it up.
	stub := &queueStubProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	provider := NewQueuedProvider(stub, 1)

	// First call holds the worker.
	firstDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(context.Background(),
			[]Message{{Role: "user", Content: "running"}})
		close(firstDone)
	}()
	select {
	case <-stub.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}

	// Second call: queued, then we cancel its context.
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := provider.Complete(ctx,
			[]Message{{Role: "user", Content: "queued"}})
		secondDone <- err
	}()
	// Wait a tick so the call enters the queue before we cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-secondDone:
		if err == nil {
			t.Error("expected ctx.Err() on cancelled queued call, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled call did not return")
	}

	// Release the first call so the worker exits cleanly.
	close(stub.releaseFirst)
	<-firstDone
}
