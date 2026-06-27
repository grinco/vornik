package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type queueStubProvider struct {
	mu           sync.Mutex
	order        []string
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (s *queueStubProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return s.CompleteWithTools(ctx, messages, nil)
}

func (s *queueStubProvider) CompleteWithTools(ctx context.Context, messages []Message, _ []Tool) (*ChatResponse, error) {
	id := messages[0].Content
	s.mu.Lock()
	s.order = append(s.order, id)
	isFirst := len(s.order) == 1
	s.mu.Unlock()

	if isFirst {
		close(s.firstStarted)
		select {
		case <-s.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &ChatResponse{Model: "stub"}, nil
}

func (s *queueStubProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, _ StreamCallback) (*ChatResponse, error) {
	return s.CompleteWithTools(ctx, messages, tools)
}

func (s *queueStubProvider) Model() string         { return "stub" }
func (s *queueStubProvider) SetMetrics(_ *Metrics) {}

func TestQueuedProviderOrdersBacklogByPriority(t *testing.T) {
	stub := &queueStubProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	provider := NewQueuedProvider(stub, 1)

	ctx := context.Background()
	firstDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(ctx, []Message{{Role: "user", Content: "running-low"}})
		close(firstDone)
	}()

	select {
	case <-stub.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}

	lowDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(WithRequestPriority(ctx, 10), []Message{{Role: "user", Content: "queued-low"}})
		close(lowDone)
	}()
	time.Sleep(20 * time.Millisecond)

	highDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(WithRequestPriority(ctx, 1), []Message{{Role: "user", Content: "queued-high"}})
		close(highDone)
	}()
	time.Sleep(20 * time.Millisecond)

	dispatcherDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(WithRequestPriority(ctx, 0), []Message{{Role: "user", Content: "queued-dispatcher"}})
		close(dispatcherDone)
	}()
	time.Sleep(20 * time.Millisecond)

	close(stub.releaseFirst)

	for _, done := range []chan struct{}{firstDone, dispatcherDone, highDone, lowDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("queued request did not finish")
		}
	}

	stub.mu.Lock()
	got := append([]string(nil), stub.order...)
	stub.mu.Unlock()
	want := []string{"running-low", "queued-dispatcher", "queued-high", "queued-low"}
	if len(got) != len(want) {
		t.Fatalf("order length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order mismatch: got %v want %v", got, want)
		}
	}
}

func TestQueuedProviderEmitsQueueMetrics(t *testing.T) {
	stub := &queueStubProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	provider := NewQueuedProvider(stub, 1)
	metrics := NewMetrics(prometheus.NewRegistry())
	provider.SetMetrics(metrics)

	ctx := context.Background()

	firstDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(ctx, []Message{{Role: "user", Content: "first"}})
		close(firstDone)
	}()

	// Wait for the first call to actually start (worker has popped it
	// and is blocked inside the stub). At this point depth should be
	// 0 and in_flight should be 1.
	select {
	case <-stub.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first call did not start")
	}
	if got := testutil.ToFloat64(metrics.QueueInFlight); got != 1 {
		t.Fatalf("after first call started: QueueInFlight=%v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.QueueDepth); got != 0 {
		t.Fatalf("after first call started: QueueDepth=%v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.QueueCallsTotal.WithLabelValues("started")); got != 1 {
		t.Fatalf("after first call started: QueueCallsTotal[started]=%v, want 1", got)
	}

	// Enqueue two more behind the blocked first call. Both should be
	// counted as queue depth = 2 (still waiting), in_flight = 1
	// (the first is still running).
	secondDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(ctx, []Message{{Role: "user", Content: "second"}})
		close(secondDone)
	}()
	thirdDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(ctx, []Message{{Role: "user", Content: "third"}})
		close(thirdDone)
	}()

	// Poll until both backlog entries land in the queue — go's
	// scheduler doesn't promise instant goroutine progress.
	if err := waitFor(time.Second, func() bool {
		return testutil.ToFloat64(metrics.QueueDepth) == 2
	}); err != nil {
		t.Fatalf("queue depth did not reach 2: depth=%v",
			testutil.ToFloat64(metrics.QueueDepth))
	}

	// Release the first call. The backlog drains through the same
	// worker; depth and in_flight return to 0 once all three finish.
	close(stub.releaseFirst)
	for _, done := range []chan struct{}{firstDone, secondDone, thirdDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("call did not finish after release")
		}
	}

	if got := testutil.ToFloat64(metrics.QueueInFlight); got != 0 {
		t.Fatalf("after drain: QueueInFlight=%v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.QueueDepth); got != 0 {
		t.Fatalf("after drain: QueueDepth=%v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.QueueCallsTotal.WithLabelValues("started")); got != 3 {
		t.Fatalf("after drain: QueueCallsTotal[started]=%v, want 3", got)
	}
	// No cancels in the happy path.
	if got := testutil.ToFloat64(metrics.QueueCallsTotal.WithLabelValues("canceled")); got != 0 {
		t.Fatalf("after drain: QueueCallsTotal[canceled]=%v, want 0", got)
	}
	// Wait-time histogram should have recorded exactly 3 observations.
	if got := testutil.CollectAndCount(metrics.QueueWaitSeconds); got != 1 {
		t.Fatalf("QueueWaitSeconds collect-count=%d, want 1 (single histogram)", got)
	}
}

func TestQueuedProviderCountsCanceledWaiter(t *testing.T) {
	stub := &queueStubProvider{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	provider := NewQueuedProvider(stub, 1)
	metrics := NewMetrics(prometheus.NewRegistry())
	provider.SetMetrics(metrics)

	ctx := context.Background()

	firstDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(ctx, []Message{{Role: "user", Content: "blocker"}})
		close(firstDone)
	}()

	select {
	case <-stub.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("blocker did not start")
	}

	// Queue a waiter, then cancel its context before the worker can
	// reach it. The waiter exits via the ctx.Done branch of do(),
	// which removes it from the heap and bumps the canceled counter.
	cancelCtx, cancel := context.WithCancel(ctx)
	waiterDone := make(chan struct{})
	go func() {
		_, _ = provider.Complete(cancelCtx, []Message{{Role: "user", Content: "waiter"}})
		close(waiterDone)
	}()
	if err := waitFor(time.Second, func() bool {
		return testutil.ToFloat64(metrics.QueueDepth) == 1
	}); err != nil {
		t.Fatalf("waiter never reached the queue: depth=%v",
			testutil.ToFloat64(metrics.QueueDepth))
	}

	cancel()
	select {
	case <-waiterDone:
	case <-time.After(time.Second):
		t.Fatal("canceled waiter never returned")
	}

	if got := testutil.ToFloat64(metrics.QueueCallsTotal.WithLabelValues("canceled")); got != 1 {
		t.Fatalf("after cancel: QueueCallsTotal[canceled]=%v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.QueueDepth); got != 0 {
		t.Fatalf("after cancel: QueueDepth=%v, want 0", got)
	}

	// Release the blocker so the test cleans up.
	close(stub.releaseFirst)
	<-firstDone
}

// waitFor polls cond up to timeout, returning nil when it first
// becomes true and an error if the deadline passes.
func waitFor(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return context.DeadlineExceeded
}
