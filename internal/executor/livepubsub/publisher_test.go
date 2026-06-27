package livepubsub

import (
	"context"
	"sync"
	"testing"
	"time"
)

func drainCh(ch <-chan LiveEvent, deadline time.Duration) []LiveEvent {
	out := []LiveEvent{}
	timeout := time.After(deadline)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-timeout:
			return out
		}
	}
}

func TestPublish_AssignsMonotonicSeq(t *testing.T) {
	p := New(50)
	ctx := context.Background()
	seqs := make([]int64, 5)
	for i := range seqs {
		seqs[i] = p.Publish(ctx, "exec_1", KindStepStarted, StepStartedPayload{StepID: "s"})
	}
	for i, s := range seqs {
		if s != int64(i) {
			t.Errorf("seqs[%d] = %d, want %d", i, s, int64(i))
		}
	}
}

func TestPublish_EmptyArgsAreNoOp(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	if seq := p.Publish(ctx, "", KindStepStarted, nil); seq != 0 {
		t.Errorf("empty execution_id: seq=%d", seq)
	}
	if seq := p.Publish(ctx, "exec_1", "", nil); seq != 0 {
		t.Errorf("empty kind: seq=%d", seq)
	}
}

func TestSubscribe_ReplaysFromZero(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	p.Publish(ctx, "exec_1", KindStepStarted, StepStartedPayload{StepID: "a"})
	p.Publish(ctx, "exec_1", KindStepCompleted, StepCompletedPayload{StepID: "a", Outcome: "ok"})

	ch, cancel, err := p.Subscribe("exec_1", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	got := drainCh(ch, 50*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 replay events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != KindStepStarted || got[1].Kind != KindStepCompleted {
		t.Errorf("wrong order: %v", got)
	}
}

func TestSubscribe_FanOutToMultipleSubscribers(t *testing.T) {
	p := New(10)
	ctx := context.Background()

	ch1, c1, _ := p.Subscribe("exec_1", 0)
	defer c1()
	ch2, c2, _ := p.Subscribe("exec_1", 0)
	defer c2()

	p.Publish(ctx, "exec_1", KindFileEdit, FileEditPayload{Path: "x.md"})
	p.Publish(ctx, "exec_1", KindFileEdit, FileEditPayload{Path: "y.md"})

	got1 := drainCh(ch1, 50*time.Millisecond)
	got2 := drainCh(ch2, 50*time.Millisecond)
	if len(got1) != 2 || len(got2) != 2 {
		t.Errorf("expected 2 events on both channels, got %d/%d", len(got1), len(got2))
	}
}

func TestSubscribe_DifferentExecutionsAreIsolated(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	chA, cancelA, _ := p.Subscribe("exec_A", 0)
	defer cancelA()
	chB, cancelB, _ := p.Subscribe("exec_B", 0)
	defer cancelB()

	p.Publish(ctx, "exec_A", KindStepStarted, StepStartedPayload{StepID: "a-step"})
	p.Publish(ctx, "exec_B", KindStepStarted, StepStartedPayload{StepID: "b-step"})

	gotA := drainCh(chA, 50*time.Millisecond)
	gotB := drainCh(chB, 50*time.Millisecond)
	if len(gotA) != 1 || gotA[0].ExecutionID != "exec_A" {
		t.Errorf("A stream wrong: %+v", gotA)
	}
	if len(gotB) != 1 || gotB[0].ExecutionID != "exec_B" {
		t.Errorf("B stream wrong: %+v", gotB)
	}
}

func TestSubscribe_RingOverflowDropsOldest(t *testing.T) {
	p := New(3)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p.Publish(ctx, "exec_1", KindStepStarted, StepStartedPayload{StepID: "s"})
	}
	// Subscribe AFTER overflow. Ring keeps the last 3 (seq 2/3/4).
	ch, cancel, _ := p.Subscribe("exec_1", 0)
	defer cancel()
	got := drainCh(ch, 50*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("expected 3 events in ring, got %d", len(got))
	}
	if got[0].Seq != 2 || got[2].Seq != 4 {
		t.Errorf("expected seqs 2..4, got %d/%d/%d", got[0].Seq, got[1].Seq, got[2].Seq)
	}
}

func TestSubscribe_GapMarkerForStaleCursor(t *testing.T) {
	p := New(3)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p.Publish(ctx, "exec_1", KindStepStarted, nil)
	}
	// Ring has seqs 2..4; ask for seq 1 (already dropped).
	ch, cancel, _ := p.Subscribe("exec_1", 1)
	defer cancel()
	got := drainCh(ch, 50*time.Millisecond)
	if len(got) == 0 {
		t.Fatal("expected at least a gap marker")
	}
	if got[0].Kind != KindReplayGap {
		t.Errorf("expected first event to be replay_gap, got %q", got[0].Kind)
	}
	payload, ok := got[0].Payload.(ReplayGapPayload)
	if !ok || payload.OldestSeq != 2 {
		t.Errorf("expected OldestSeq=2, got %+v", got[0].Payload)
	}
}

func TestSubscribe_FromFutureSeqWaitsForNewEvents(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	p.Publish(ctx, "exec_1", KindStepStarted, nil)
	p.Publish(ctx, "exec_1", KindStepStarted, nil)

	ch, cancel, _ := p.Subscribe("exec_1", 5) // future seq
	defer cancel()
	// No replay should fire; channel should be empty.
	got := drainCh(ch, 20*time.Millisecond)
	if len(got) != 0 {
		t.Errorf("expected no replay for future seq, got %d events", len(got))
	}
	// Publish a new event AFTER subscription — should land.
	p.Publish(ctx, "exec_1", KindStepCompleted, nil)
	got = drainCh(ch, 50*time.Millisecond)
	if len(got) != 1 || got[0].Kind != KindStepCompleted {
		t.Errorf("expected one new event, got %+v", got)
	}
}

func TestSubscribe_CancelRemovesSubscriber(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	ch, cancel, _ := p.Subscribe("exec_1", 0)
	cancel()
	// Publish after cancel shouldn't reach the channel.
	p.Publish(ctx, "exec_1", KindStepStarted, nil)
	got := drainCh(ch, 20*time.Millisecond)
	for _, e := range got {
		if e.Kind == KindStepStarted {
			t.Errorf("cancelled subscriber should not receive post-cancel events")
		}
	}
	// Double-cancel is idempotent.
	cancel()
}

func TestPublish_NonBlockingOnSlowSubscriber(t *testing.T) {
	p := New(10)
	ctx := context.Background()
	_, cancel, _ := p.Subscribe("exec_1", 0)
	defer cancel()
	// Don't drain the channel; Publish must not block on the
	// (capacity 64) channel even when it fills.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			p.Publish(ctx, "exec_1", KindLLMToken, LLMTokenPayload{Delta: "x"})
		}
	}()
	select {
	case <-done:
		// Good — finished quickly.
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}
}

func TestPublish_ConcurrentSafe(t *testing.T) {
	p := New(50)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				p.Publish(ctx, "exec_1", KindStepStarted, nil)
			}
		}()
	}
	wg.Wait()
	ch, cancel, _ := p.Subscribe("exec_1", 0)
	defer cancel()
	got := drainCh(ch, 50*time.Millisecond)
	// We sent 100 events into a ring of 50; replay should be 50.
	if len(got) != 50 {
		t.Errorf("expected ring of 50 after 100 concurrent publishes, got %d", len(got))
	}
}

func TestSweeper_EvictsIdleStreams(t *testing.T) {
	p, stop := NewWithSweeper(10, 30*time.Millisecond)
	defer stop()
	impl := p.(*inProcessPublisher)
	ctx := context.Background()
	p.Publish(ctx, "exec_idle", KindStepStarted, nil)
	// Force the lastActive timestamp into the past so the next
	// sweep evicts it.
	impl.mu.Lock()
	impl.streams["exec_idle"].mu.Lock()
	impl.streams["exec_idle"].lastActive = time.Now().Add(-time.Hour)
	impl.streams["exec_idle"].mu.Unlock()
	impl.mu.Unlock()

	time.Sleep(80 * time.Millisecond)

	impl.mu.Lock()
	_, present := impl.streams["exec_idle"]
	impl.mu.Unlock()
	if present {
		t.Error("expected idle stream to be evicted")
	}
}

func TestSubscribe_EmptyExecutionIDErrors(t *testing.T) {
	p := New(10)
	if _, _, err := p.Subscribe("", 0); err == nil {
		t.Error("expected error on empty execution_id")
	}
}
