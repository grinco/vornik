package email

import (
	"context"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// fenceGate is a leader gate that ALSO implements the
// leaderelection.EpochVerifier shape (VerifyEpoch). The fence helper
// (leaderelection.DangerousWriteAllowed) type-asserts the gate to a
// verifier; a verifier reporting ok=false is a stale/superseded leader
// and must NOT perform the dangerous write (processing the fetched
// batch + MarkSeen). leader gates that expose only IsLeader() keep the
// pre-fence behaviour (the existing stubLeaderGate covers that case).
type fenceGate struct {
	leader   atomic.Bool
	epochOK  atomic.Bool
	verified atomic.Int32
}

func (g *fenceGate) IsLeader() bool { return g.leader.Load() }

func (g *fenceGate) VerifyEpoch(_ context.Context) (ok bool, current int64, err error) {
	g.verified.Add(1)
	return g.epochOK.Load(), 1, nil
}

// fenceIMAP counts FetchUnseen + MarkSeen so a test can assert whether
// the dangerous batch-processing path ran. FetchUnseen returns one
// message so MarkSeen would fire on the happy path; the message body
// is a minimal RFC 5322 envelope that ParseRFC5322 accepts.
type fenceIMAP struct {
	fetchCalls    int32
	markSeenCalls int32
}

const fenceRawMessage = "From: alice@example.com\r\n" +
	"To: bot@example.com\r\n" +
	"Subject: hello\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"\r\n" +
	"body text\r\n"

func (c *fenceIMAP) Connect(_ context.Context, _ IMAPDialConfig) error { return nil }
func (c *fenceIMAP) Close() error                                      { return nil }
func (c *fenceIMAP) FetchUnseen(_ context.Context) ([]RawMessage, error) {
	atomic.AddInt32(&c.fetchCalls, 1)
	return []RawMessage{{UID: "1", Body: []byte(fenceRawMessage)}}, nil
}
func (c *fenceIMAP) MarkSeen(_ context.Context, _ string) error {
	atomic.AddInt32(&c.markSeenCalls, 1)
	return nil
}

// fenceRecorder is a no-op Receiver that records whether Receive ran
// (i.e. whether the fetched batch was actually processed into a reply/
// task forward). A stale leader must never reach this.
type fenceRecorder struct{ received int32 }

func (r *fenceRecorder) Receive(_ context.Context, _ conversation.ChannelMessage) error {
	atomic.AddInt32(&r.received, 1)
	return nil
}

// newFenceChannel wires a Channel with the fence IMAP + gate and binds
// a recorder Receiver so processing is observable.
func newFenceChannel(t *testing.T, imap IMAPClient, gate LeaderGate) (*Channel, *fenceRecorder) {
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
	rec := &fenceRecorder{}
	ch.recv = rec
	return ch, rec
}

// TestRunPollCycle_StaleLeader_SkipsBatch pins review finding B1: a
// leader whose TTL expired and was superseded (IsLeader()=true locally
// but VerifyEpoch ok=false) must NOT process the fetched batch — no
// Receive, no MarkSeen — so the real leader handles the messages and we
// never double-reply.
func TestRunPollCycle_StaleLeader_SkipsBatch(t *testing.T) {
	imap := &fenceIMAP{}
	gate := &fenceGate{}
	gate.leader.Store(true)   // still thinks it's leader
	gate.epochOK.Store(false) // but a newer epoch has superseded it
	ch, rec := newFenceChannel(t, imap, gate)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&imap.markSeenCalls); got != 0 {
		t.Errorf("stale leader called MarkSeen %d times; want 0 (must leave messages unseen for the real leader)", got)
	}
	if got := atomic.LoadInt32(&rec.received); got != 0 {
		t.Errorf("stale leader forwarded %d messages; want 0 (must not process the batch)", got)
	}
	if got := gate.verified.Load(); got == 0 {
		t.Errorf("fence did not consult VerifyEpoch")
	}
}

// TestRunPollCycle_CurrentLeader_ProcessesBatch confirms the fence
// opens cleanly: a verifier reporting ok=true processes the batch
// (Receive + MarkSeen fire) exactly as before the fence.
func TestRunPollCycle_CurrentLeader_ProcessesBatch(t *testing.T) {
	imap := &fenceIMAP{}
	gate := &fenceGate{}
	gate.leader.Store(true)
	gate.epochOK.Store(true)
	ch, rec := newFenceChannel(t, imap, gate)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&rec.received); got != 1 {
		t.Errorf("current leader forwarded %d messages; want 1", got)
	}
	if got := atomic.LoadInt32(&imap.markSeenCalls); got != 1 {
		t.Errorf("current leader called MarkSeen %d times; want 1", got)
	}
}

// TestRunPollCycle_NonVerifierGate_ProcessesBatch confirms a plain
// IsLeader-only gate (no VerifyEpoch) keeps pre-fence behaviour: the
// fence helper proceeds when the gate is not an EpochVerifier.
func TestRunPollCycle_NonVerifierGate_ProcessesBatch(t *testing.T) {
	imap := &fenceIMAP{}
	gate := &stubLeaderGate{}
	gate.leader.Store(true)
	ch, rec := newFenceChannel(t, imap, gate)

	ch.runPollCycle(context.Background())

	if got := atomic.LoadInt32(&rec.received); got != 1 {
		t.Errorf("non-verifier leader forwarded %d messages; want 1", got)
	}
	if got := atomic.LoadInt32(&imap.markSeenCalls); got != 1 {
		t.Errorf("non-verifier leader called MarkSeen %d times; want 1", got)
	}
}
