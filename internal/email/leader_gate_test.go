package email

import (
	"context"
	"sync/atomic"
	"testing"
)

// stubLeaderGate is the minimal LeaderGate impl. Flipping
// leader flips the gate atomically so a test can simulate
// failover.
type stubLeaderGate struct {
	leader atomic.Bool
}

func (s *stubLeaderGate) IsLeader() bool { return s.leader.Load() }

// TestRunPollCycle_SkipsFetchWhenNotLeader pins the cluster
// contract: non-leader replicas must NOT call IMAP.FetchUnseen,
// because every replica having FetchUnseen + MarkSeen race
// would double-process messages and double-reply the user.
func TestRunPollCycle_SkipsFetchWhenNotLeader(t *testing.T) {
	imap := newCountingIMAP()
	gate := &stubLeaderGate{} // leader=false
	ch := newTestChannelWithGate(t, imap, gate)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&imap.fetchCalls); got != 0 {
		t.Errorf("non-leader runPollCycle called FetchUnseen %d times; want 0", got)
	}
}

// TestRunPollCycle_FetchesWhenLeader confirms the gate opens
// cleanly — leader=true must let the cycle run normally.
func TestRunPollCycle_FetchesWhenLeader(t *testing.T) {
	imap := newCountingIMAP()
	gate := &stubLeaderGate{}
	gate.leader.Store(true)
	ch := newTestChannelWithGate(t, imap, gate)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&imap.fetchCalls); got != 1 {
		t.Errorf("leader runPollCycle called FetchUnseen %d times; want 1", got)
	}
}

// TestRunPollCycle_NilGate_PassesThrough confirms the legacy
// behaviour: when no gate is wired (single-process default),
// every cycle fetches.
func TestRunPollCycle_NilGate_PassesThrough(t *testing.T) {
	imap := newCountingIMAP()
	ch := newTestChannelWithGate(t, imap, nil)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&imap.fetchCalls); got != 1 {
		t.Errorf("nil-gate cycle should fetch unconditionally; got %d calls", got)
	}
}

// --- helpers ---

// countingIMAP is the lightweight stub: it satisfies the
// IMAPClient surface the poll cycle touches and counts fetches.
type countingIMAP struct {
	fetchCalls int32
}

func newCountingIMAP() *countingIMAP { return &countingIMAP{} }

func (c *countingIMAP) Connect(_ context.Context, _ IMAPDialConfig) error { return nil }
func (c *countingIMAP) Close() error                                      { return nil }
func (c *countingIMAP) FetchUnseen(_ context.Context) ([]RawMessage, error) {
	atomic.AddInt32(&c.fetchCalls, 1)
	return nil, nil
}
func (c *countingIMAP) MarkSeen(_ context.Context, _ string) error { return nil }

// newTestChannelWithGate constructs a minimally-wired Channel
// for runPollCycle exercises. cfg fields not touched by the
// cycle are left zero; the test only cares about the gate +
// IMAP stub.
func newTestChannelWithGate(t *testing.T, imap IMAPClient, gate LeaderGate) *Channel {
	t.Helper()
	ch, err := New(Config{
		IMAPHost:     "imap.test",
		IMAPUsername: "u",
		IMAPPassword: "p",
		IMAPClient:   imap,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch.SetLeaderGate(gate)
	return ch
}
