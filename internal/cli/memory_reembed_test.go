package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/memory"
)

type stubStats struct {
	calls   atomic.Int32
	depths  []int64 // returned in order, last value sticks
	missing bool    // when true, returns no row for the project
	err     error
}

func (s *stubStats) Stats(_ context.Context) ([]memory.ProjectMemoryStats, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	if s.missing {
		return []memory.ProjectMemoryStats{{ProjectID: "other", QueueDepth: 0}}, nil
	}
	idx := int(s.calls.Load()) - 1
	if idx >= len(s.depths) {
		idx = len(s.depths) - 1
	}
	return []memory.ProjectMemoryStats{
		{ProjectID: "p", ChunksTotal: 10, ChunksEmbedded: 10 - s.depths[idx], QueueDepth: s.depths[idx]},
	}, nil
}

func TestWatchReembedProgress_DrainsToZero(t *testing.T) {
	st := &stubStats{depths: []int64{5, 2, 0}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := watchReembedProgress(ctx, st, "p", 10, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if st.calls.Load() < 3 {
		t.Fatalf("expected ≥3 stats calls, got %d", st.calls.Load())
	}
}

func TestWatchReembedProgress_ImmediateZero(t *testing.T) {
	st := &stubStats{depths: []int64{0}}
	err := watchReembedProgress(context.Background(), st, "p", 10, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if st.calls.Load() != 1 {
		t.Fatalf("expected 1 stats call (early exit), got %d", st.calls.Load())
	}
}

func TestWatchReembedProgress_ProjectMissing(t *testing.T) {
	st := &stubStats{missing: true}
	err := watchReembedProgress(context.Background(), st, "p", 10, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Missing → treated as drained → exits after one call.
	if st.calls.Load() != 1 {
		t.Fatalf("expected 1 stats call, got %d", st.calls.Load())
	}
}

func TestWatchReembedProgress_ContextCancelDetaches(t *testing.T) {
	st := &stubStats{depths: []int64{5, 5, 5, 5}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := watchReembedProgress(ctx, st, "p", 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("cancel should be clean exit: %v", err)
	}
}

func TestWatchReembedProgress_StatsErrorToleratedAndRetried(t *testing.T) {
	// One transient error then a clean drain.
	st := &stubStats{err: errors.New("transient")}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	// Error path: keeps polling until ctx times out.
	err := watchReembedProgress(ctx, st, "p", 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("err should be swallowed until ctx cancel: %v", err)
	}
}

func TestWatchReembedProgress_DefaultIntervalApplied(t *testing.T) {
	st := &stubStats{depths: []int64{0}}
	// interval=0 → defaulted to 3s, but the first sample drains so we
	// exit immediately without waiting.
	err := watchReembedProgress(context.Background(), st, "p", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// negativeDepthStub returns more chunks than the initial enqueue —
// catches the done<0 clamp in the progress printer.
type negativeDepthStub struct{}

func (negativeDepthStub) Stats(context.Context) ([]memory.ProjectMemoryStats, error) {
	return []memory.ProjectMemoryStats{{ProjectID: "p", QueueDepth: 99}}, nil
}

func TestWatchReembedProgress_NegativeDoneClamped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := watchReembedProgress(ctx, negativeDepthStub{}, "p", 5, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
