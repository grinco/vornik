package email

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// reconnectFakeIMAP extends fakeIMAP with the IMAPReconnector
// interface so the reconnect-on-drop tests can drive it. Tracks the
// number of Reconnect calls so the rate-limit test can assert.
type reconnectFakeIMAP struct {
	*fakeIMAP

	reconnectMu    sync.Mutex
	reconnectCalls int
	reconnectErr   error
	// fetchScript queues errors / success returns. Each FetchUnseen
	// pops the next entry; once empty, FetchUnseen returns (nil, nil).
	fetchScript []fetchScriptEntry
	scriptIdx   int
}

type fetchScriptEntry struct {
	err  error
	msgs []RawMessage
}

func newReconnectFake() *reconnectFakeIMAP {
	return &reconnectFakeIMAP{fakeIMAP: newFakeIMAP()}
}

func (f *reconnectFakeIMAP) FetchUnseen(_ context.Context) ([]RawMessage, error) {
	// Update the underlying fakeIMAP's counter so the existing
	// waitFor probes that check fetchCalls work uniformly.
	f.mu.Lock()
	f.fetchCalls++
	f.mu.Unlock()

	f.reconnectMu.Lock()
	defer f.reconnectMu.Unlock()
	if f.scriptIdx >= len(f.fetchScript) {
		return nil, nil
	}
	entry := f.fetchScript[f.scriptIdx]
	f.scriptIdx++
	return entry.msgs, entry.err
}

func (f *reconnectFakeIMAP) Reconnect(_ context.Context) error {
	f.reconnectMu.Lock()
	defer f.reconnectMu.Unlock()
	f.reconnectCalls++
	return f.reconnectErr
}

func (f *reconnectFakeIMAP) reconnectCount() int {
	f.reconnectMu.Lock()
	defer f.reconnectMu.Unlock()
	return f.reconnectCalls
}

// ---- isTransportError classification ----

func TestIsTransportError_NilNotTransport(t *testing.T) {
	if isTransportError(nil) {
		t.Error("nil error should not be classified transport")
	}
}

func TestIsTransportError_EOFIsTransport(t *testing.T) {
	if !isTransportError(io.EOF) {
		t.Error("io.EOF should be transport")
	}
}

func TestIsTransportError_UnexpectedEOFIsTransport(t *testing.T) {
	if !isTransportError(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF should be transport")
	}
}

func TestIsTransportError_ConnectionClosedIsTransport(t *testing.T) {
	err := errors.New("use of closed network connection")
	if !isTransportError(err) {
		t.Error("use-of-closed should be transport")
	}
}

func TestIsTransportError_BrokenPipeIsTransport(t *testing.T) {
	err := errors.New("write tcp 127.0.0.1: write: broken pipe")
	if !isTransportError(err) {
		t.Error("broken pipe should be transport")
	}
}

func TestIsTransportError_GenericNotTransport(t *testing.T) {
	if isTransportError(errors.New("UID SEARCH: invalid criteria")) {
		t.Error("non-transport error should not be classified transport")
	}
}

// ---- reconnect rate limiter ----

func TestReconnectLimiter_AllowsUnderCap(t *testing.T) {
	rl := newReconnectLimiter(3, time.Minute, func() time.Time { return time.Unix(1000, 0) })
	for i := 0; i < 3; i++ {
		if !rl.tryAcquire() {
			t.Errorf("attempt %d denied; should be under cap", i)
		}
	}
}

func TestReconnectLimiter_RejectsOverCap(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newReconnectLimiter(3, time.Minute, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		_ = rl.tryAcquire()
	}
	if rl.tryAcquire() {
		t.Error("attempt 4 within window should be denied")
	}
}

func TestReconnectLimiter_RolloverAfterWindow(t *testing.T) {
	current := time.Unix(1000, 0)
	rl := newReconnectLimiter(2, time.Minute, func() time.Time { return current })
	_ = rl.tryAcquire()
	_ = rl.tryAcquire()
	if rl.tryAcquire() {
		t.Error("3rd within window should be denied")
	}
	// Advance past the window.
	current = current.Add(2 * time.Minute)
	if !rl.tryAcquire() {
		t.Error("after window rollover, attempt should succeed")
	}
}

func TestReconnectLimiter_ZeroLimitDeniesAll(t *testing.T) {
	rl := newReconnectLimiter(0, time.Minute, func() time.Time { return time.Now() })
	if rl.tryAcquire() {
		t.Error("zero cap should deny all")
	}
}

// ---- channel runPollCycle reconnect-on-drop ----

func TestRunPollCycle_ReconnectsOnTransportError(t *testing.T) {
	cfg := validConfig()
	imap := newReconnectFake()
	// First fetch returns a transport error; reconnect succeeds;
	// retry fetch returns one message.
	imap.fetchScript = []fetchScriptEntry{
		{err: io.EOF},
		{msgs: []RawMessage{
			{UID: "1", Body: sampleEmail("a@b", "alice@ext.test", "Hello", "body")},
		}},
	}
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	if imap.reconnectCount() != 1 {
		t.Errorf("Reconnect calls = %d, want 1", imap.reconnectCount())
	}
	cancel()
	<-errCh
}

func TestRunPollCycle_DoesNotReconnectOnNonTransportError(t *testing.T) {
	cfg := validConfig()
	imap := newReconnectFake()
	imap.fetchScript = []fetchScriptEntry{
		{err: errors.New("UID SEARCH: invalid criteria")},
	}
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Give the first cycle a chance to run.
	waitFor(t, func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return imap.fetchCalls >= 1
	})
	time.Sleep(20 * time.Millisecond)
	if imap.reconnectCount() != 0 {
		t.Errorf("Reconnect calls = %d, want 0 for non-transport error", imap.reconnectCount())
	}
	cancel()
	<-errCh
}

func TestRunPollCycle_RateLimitsReconnects(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = 5 * time.Millisecond
	imap := newReconnectFake()
	// Inject many transport errors in a row.
	for i := 0; i < 10; i++ {
		imap.fetchScript = append(imap.fetchScript, fetchScriptEntry{err: io.EOF})
	}
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Wait for at least 5 fetch attempts to fire.
	waitFor(t, func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return imap.fetchCalls >= 5
	})
	// Reconnect cap is 3/minute by default — the channel must not
	// have hammered Reconnect more than that even though every fetch
	// returns a transport drop.
	if got := imap.reconnectCount(); got > 3 {
		t.Errorf("Reconnect count = %d, exceeds 3-per-minute cap", got)
	}
	cancel()
	<-errCh
}

func TestRunPollCycle_ReconnectErrorDoesNotAbortLoop(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = 5 * time.Millisecond
	imap := newReconnectFake()
	imap.reconnectErr = errors.New("dial refused")
	imap.fetchScript = []fetchScriptEntry{
		{err: io.EOF},
		{err: io.EOF},
		{msgs: []RawMessage{
			{UID: "9", Body: sampleEmail("a@b", "alice@ext.test", "S", "B")},
		}},
	}
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Even though reconnect fails, the loop should continue and
	// eventually pick up the message (subsequent fetches succeed).
	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	cancel()
	<-errCh
}

// noReconnectIMAP exposes IMAPClient WITHOUT IMAPReconnector. The
// channel must accept these legacy clients (slice 1 fakes don't
// implement Reconnect) without crashing.
type noReconnectIMAP struct {
	*fakeIMAP
}

func TestRunPollCycle_LegacyClientWithoutReconnectInterface(t *testing.T) {
	cfg := validConfig()
	inner := newFakeIMAP()
	inner.fetchErr = io.EOF
	imap := &noReconnectIMAP{fakeIMAP: inner}
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Just verify the loop doesn't crash even though the client
	// can't be reconnected — fetch error is swallowed and the loop
	// continues.
	waitFor(t, func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return imap.fetchCalls >= 1
	})
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after cancel with no-reconnect client")
	}
}

// ---- emersion adapter Reconnect smoke test ----

func TestEmersionIMAPClient_ReconnectBeforeConnectReturnsError(t *testing.T) {
	// Reconnect without a prior Connect must error rather than
	// trying to redial against zero-valued config.
	c := NewIMAPClient().(*emersionIMAPClient)
	err := c.Reconnect(context.Background())
	if err == nil {
		t.Error("Reconnect before Connect must error")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("err = %v, want to mention not-connected", err)
	}
}
