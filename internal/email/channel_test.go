package email

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// validConfig returns a Config seeded with safe defaults for tests
// that don't care about specific allowlist contents.
func validConfig() Config {
	return Config{
		IMAPHost:     "imap.test",
		IMAPPort:     993,
		IMAPUsername: "vornik@test",
		IMAPPassword: "shhh",
		SMTPHost:     "smtp.test",
		SMTPPort:     587,
		SMTPUsername: "vornik@test",
		SMTPPassword: "shhh",
		FromAddress:  "vornik@test",
		PollInterval: 10 * time.Millisecond,
	}
}

// fakeIMAP is the in-memory IMAPClient fake the channel tests
// drive. It supports queueing messages for a single fetch cycle,
// recording MarkSeen calls, and injecting Connect/Fetch errors.
type fakeIMAP struct {
	mu sync.Mutex

	connectErr error
	closeErr   error

	// queue is consumed FIFO on each FetchUnseen — one batch per
	// poll cycle. After the queue empties, subsequent calls return
	// (nil, nil).
	queue [][]RawMessage
	// fetchErr injected once on the next FetchUnseen call.
	fetchErr error

	markSeen     []string
	markSeenErr  error
	connectCalls int
	closeCalls   int
	fetchCalls   int

	// connectGate optionally blocks Connect until the test signals
	// — used to assert "Stop unblocks an in-flight Start." The
	// signal is the channel close on this field.
	connectGate chan struct{}
}

func newFakeIMAP(batches ...[]RawMessage) *fakeIMAP {
	return &fakeIMAP{queue: batches}
}

func (f *fakeIMAP) Connect(ctx context.Context, cfg IMAPDialConfig) error {
	f.mu.Lock()
	f.connectCalls++
	gate := f.connectGate
	err := f.connectErr
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeIMAP) FetchUnseen(ctx context.Context) ([]RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchCalls++
	if f.fetchErr != nil {
		err := f.fetchErr
		f.fetchErr = nil
		return nil, err
	}
	if len(f.queue) == 0 {
		return nil, nil
	}
	batch := f.queue[0]
	f.queue = f.queue[1:]
	return batch, nil
}

func (f *fakeIMAP) MarkSeen(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markSeenErr != nil {
		return f.markSeenErr
	}
	f.markSeen = append(f.markSeen, uid)
	return nil
}

func (f *fakeIMAP) Close() error {
	f.mu.Lock()
	f.closeCalls++
	err := f.closeErr
	f.mu.Unlock()
	return err
}

func (f *fakeIMAP) snapshotMarkSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.markSeen))
	copy(out, f.markSeen)
	return out
}

// fakeSMTP captures every Send call so tests can inspect the
// rendered RFC 5322 payload.
type fakeSMTP struct {
	mu       sync.Mutex
	requests []SMTPSendRequest
	sendErr  error
	closeErr error
}

func (f *fakeSMTP) Send(_ context.Context, req SMTPSendRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.requests = append(f.requests, req)
	return nil
}

func (f *fakeSMTP) Close() error { return f.closeErr }

func (f *fakeSMTP) snapshot() []SMTPSendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SMTPSendRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// captureReceiver is a conversation.Receiver that stashes every
// inbound message for assertion.
type captureReceiver struct {
	mu  sync.Mutex
	got []conversation.ChannelMessage
	err error
}

func (r *captureReceiver) Receive(_ context.Context, msg conversation.ChannelMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, msg)
	return nil
}

func (r *captureReceiver) snapshot() []conversation.ChannelMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]conversation.ChannelMessage, len(r.got))
	copy(out, r.got)
	return out
}

// ---- constructor + interface guards ----

func TestNew_RejectsEmptyIMAPHost(t *testing.T) {
	cfg := validConfig()
	cfg.IMAPHost = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with empty IMAPHost returned nil error")
	}
}

func TestNew_RejectsEmptyIMAPUsername(t *testing.T) {
	cfg := validConfig()
	cfg.IMAPUsername = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with empty IMAPUsername returned nil error")
	}
}

func TestNew_RejectsEmptyIMAPPassword(t *testing.T) {
	cfg := validConfig()
	cfg.IMAPPassword = ""
	if _, err := New(cfg); err == nil {
		t.Fatal("New with empty IMAPPassword returned nil error")
	}
}

func TestNew_DefaultsPollIntervalAndMailbox(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = 0
	cfg.IMAPMailbox = ""
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ch.pollInterval != defaultPollInterval {
		t.Errorf("pollInterval = %v, want %v", ch.pollInterval, defaultPollInterval)
	}
	if ch.mailbox != "INBOX" {
		t.Errorf("mailbox = %q, want INBOX", ch.mailbox)
	}
}

func TestNew_UsesNoopVerifierWhenNil(t *testing.T) {
	ch, err := New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := ch.verifier.(NoopSignatureVerifier); !ok {
		t.Errorf("verifier = %T, want NoopSignatureVerifier", ch.verifier)
	}
}

func TestChannel_Name(t *testing.T) {
	ch, _ := New(validConfig())
	if got := ch.Name(); got != "email" {
		t.Errorf("Name() = %q, want email", got)
	}
}

func TestChannel_SatisfiesInterface(t *testing.T) {
	ch, _ := New(validConfig())
	var _ conversation.Channel = ch
}

// ---- inbound poll cycle ----

// sampleEmail builds a complete RFC 5322 wire payload for tests.
func sampleEmail(messageID, from, subject, body string) []byte {
	return []byte(
		"From: " + from + "\r\n" +
			"To: vornik@test\r\n" +
			"Subject: " + subject + "\r\n" +
			"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
			"Message-ID: <" + messageID + ">\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"\r\n" +
			body + "\r\n",
	)
}

// sampleEmailThread builds a reply that references an earlier
// Message-ID — tests the threading-via-References path.
func sampleEmailThread(messageID, from, subject, body, inReplyTo, references string) []byte {
	hdrs := "From: " + from + "\r\n" +
		"To: vornik@test\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		"In-Reply-To: <" + inReplyTo + ">\r\n"
	if references != "" {
		hdrs += "References: <" + references + ">\r\n"
	}
	hdrs += "Content-Type: text/plain; charset=UTF-8\r\n\r\n" + body + "\r\n"
	return []byte(hdrs)
}

func startChannel(t *testing.T, ch *Channel, recv conversation.Receiver) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- ch.Start(ctx, recv) }()
	return cancel, errCh
}

// waitFor polls cond until it returns true or 2 seconds elapse.
// Uses a short tick so flaky-tolerant assertions stay tight.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("waitFor: condition not met within 2s")
}

func TestPoller_PicksUpNewMessage(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "1", Body: sampleEmail("a@b", "alice@external.test", "Hello vornik", "Please help")},
	})
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if got.Source != "email" {
		t.Errorf("Source = %q, want email", got.Source)
	}
	if got.SpeakerID != "alice@external.test" {
		t.Errorf("SpeakerID = %q, want alice@external.test", got.SpeakerID)
	}
	if got.SessionID != "a@b" {
		t.Errorf("SessionID = %q, want a@b (Message-ID)", got.SessionID)
	}
	if !strings.Contains(got.Text, "Please help") {
		t.Errorf("Text = %q, want to contain Please help", got.Text)
	}
	if got.ChannelSpecific["subject"] != "Hello vornik" {
		t.Errorf("ChannelSpecific[subject] = %q", got.ChannelSpecific["subject"])
	}

	waitFor(t, func() bool { return len(imap.snapshotMarkSeen()) == 1 })
	cancel()
	<-errCh
}

func TestPoller_AllowlistRejectsUnlistedSender(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"trusted.com"}
	imap := newFakeIMAP([]RawMessage{
		{UID: "2", Body: sampleEmail("c@d", "stranger@evil.test", "spam", "buy now")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Wait until the IMAP fake has been polled at least once with
	// the message in queue. Then verify Receiver was NOT called.
	waitFor(t, func() bool { return len(imap.snapshotMarkSeen()) == 1 })
	if got := len(recv.snapshot()); got != 0 {
		t.Errorf("Receiver got %d messages, want 0 (allowlist rejection)", got)
	}
	cancel()
	<-errCh
}

func TestPoller_AllowlistAdmitsFullAddress(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"alice@external.test"}
	imap := newFakeIMAP([]RawMessage{
		{UID: "3", Body: sampleEmail("e@f", "alice@external.test", "hi", "body")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	cancel()
	<-errCh
}

func TestPoller_AllowlistDomainMatchIsCaseInsensitive(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"External.Test"}
	imap := newFakeIMAP([]RawMessage{
		{UID: "4", Body: sampleEmail("g@h", "ALICE@external.test", "hi", "body")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	cancel()
	<-errCh
}

func TestPoller_ThreadingViaReferences(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "10", Body: sampleEmailThread("reply-1", "alice@ext.test", "Re: Topic", "reply", "root-1", "root-1")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if got.SessionID != "root-1" {
		t.Errorf("SessionID = %q, want root-1 (References)", got.SessionID)
	}
	if got.InReplyTo != "root-1" {
		t.Errorf("InReplyTo = %q, want root-1", got.InReplyTo)
	}
	cancel()
	<-errCh
}

func TestPoller_ThreadingViaInReplyToWhenNoReferences(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "11", Body: sampleEmailThread("reply-2", "alice@ext.test", "Re: Topic", "body", "parent-2", "")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if got.SessionID != "parent-2" {
		t.Errorf("SessionID = %q, want parent-2 (In-Reply-To fallback)", got.SessionID)
	}
	cancel()
	<-errCh
}

func TestPoller_ReceiverErrorDoesNotMarkSeen(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "20", Body: sampleEmail("x@y", "alice@ext.test", "subject", "body")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{err: errors.New("dispatch failed")}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Wait for the Receiver to be hit (channel attempts delivery).
	// Since fetch only returns the message once (one-shot queue), we
	// instead poll the fetchCalls counter to be ≥1 and assert
	// MarkSeen was NOT recorded.
	waitFor(t, func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return imap.fetchCalls >= 1
	})
	// Give the per-message goroutine a moment to finalise (no
	// MarkSeen call expected, so we just check the snapshot is
	// empty after a brief settle).
	time.Sleep(20 * time.Millisecond)
	if got := imap.snapshotMarkSeen(); len(got) != 0 {
		t.Errorf("MarkSeen = %v, want empty (Receiver error must not ack)", got)
	}
	cancel()
	<-errCh
}

func TestPoller_ParseErrorMarksSeenAndDrops(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "30", Body: []byte("not a valid rfc5322")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(imap.snapshotMarkSeen()) == 1 })
	if len(recv.snapshot()) != 0 {
		t.Errorf("Receiver got messages despite parse failure")
	}
	cancel()
	<-errCh
}

// errVerifier always fails — used to assert the dropped-on-verify
// path also marks the message seen so we don't loop forever.
type errVerifier struct{}

func (errVerifier) Verify(_ context.Context, _ ParsedMessage) error {
	return errors.New("DKIM fail")
}

func TestPoller_VerifierFailureMarksSeenAndDrops(t *testing.T) {
	cfg := validConfig()
	cfg.SignatureVerifier = errVerifier{}
	imap := newFakeIMAP([]RawMessage{
		{UID: "40", Body: sampleEmail("a@b", "alice@ext.test", "subject", "body")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(imap.snapshotMarkSeen()) == 1 })
	if len(recv.snapshot()) != 0 {
		t.Errorf("Receiver got %d messages despite verifier rejection", len(recv.snapshot()))
	}
	cancel()
	<-errCh
}

func TestPoller_FetchErrorDoesNotAbortLoop(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP(
		nil, // first cycle is empty after we inject the error
		[]RawMessage{{UID: "50", Body: sampleEmail("a@b", "alice@ext.test", "subj", "body")}},
	)
	imap.fetchErr = errors.New("transient")
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// First poll cycle returns the injected error; second cycle
	// drains the queued message. The poll loop must NOT exit on
	// the transient error.
	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	cancel()
	<-errCh
}

func TestStart_StopBeforeFirstPoll(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP()
	imap.connectGate = make(chan struct{}) // block Connect forever
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}

	startDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { startDone <- ch.Start(ctx, recv) }()

	// Stop before Connect completes — ctx cancellation should
	// unblock the gate via the Connect path.
	cancel()
	select {
	case err := <-startDone:
		// Either context.Canceled or a wrapped IMAP error are
		// acceptable — both indicate the Start goroutine exited.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after context cancel")
	}
}

func TestStart_StopFnUnblocksPollLoop(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval = 1 * time.Hour // ensure Stop is what exits the loop, not a tick
	imap := newFakeIMAP()
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	startDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { startDone <- ch.Start(ctx, recv) }()

	// Wait for Connect to land.
	waitFor(t, func() bool {
		imap.mu.Lock()
		defer imap.mu.Unlock()
		return imap.connectCalls == 1
	})
	// Call Stop and assert Start returns promptly.
	if err := ch.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit after Stop()")
	}
	// Stop is idempotent.
	if err := ch.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStart_RejectsNilReceiver(t *testing.T) {
	ch, _ := New(validConfig())
	err := ch.Start(context.Background(), nil)
	if err == nil {
		t.Fatal("Start with nil Receiver returned nil error")
	}
}

func TestStart_RejectsMissingIMAPClient(t *testing.T) {
	cfg := validConfig()
	// IMAPClient deliberately not set
	ch, _ := New(cfg)
	err := ch.Start(context.Background(), &captureReceiver{})
	if err == nil {
		t.Fatal("Start with no IMAPClient returned nil error")
	}
}

func TestStart_PropagatesConnectError(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP()
	imap.connectErr = errors.New("auth rejected")
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	err := ch.Start(context.Background(), &captureReceiver{})
	if err == nil || !strings.Contains(err.Error(), "auth rejected") {
		t.Fatalf("Start err = %v, want to wrap auth rejected", err)
	}
}

// ---- outbound Send ----

func TestSend_ReturnsSentinelWhenOutboundUnconfigured(t *testing.T) {
	cfg := validConfig()
	// Leave SMTPSender nil so Send hits the unconfigured branch.
	ch, _ := New(cfg)
	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "root-1",
		Text:      "hi",
	})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("Send err = %v, want ErrOutboundNotConfigured", err)
	}
	if id != "" {
		t.Errorf("Send id = %q, want empty", id)
	}
}

func TestSend_UnknownSessionWithoutExplicitToReturnsSentinel(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{}
	cfg.SMTPSender = smtp
	ch, _ := New(cfg)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "unknown-thread",
		Text:      "hi",
	})
	if !errors.Is(err, ErrUnknownSession) {
		t.Errorf("Send err = %v, want ErrUnknownSession", err)
	}
}

func TestSend_FromSessionHistory(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{}
	imap := newFakeIMAP([]RawMessage{
		{UID: "100", Body: sampleEmail("thread-root", "alice@ext.test", "Topic", "first msg")},
	})
	cfg.IMAPClient = imap
	cfg.SMTPSender = smtp
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })

	// Now reply to the known session.
	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "thread-root",
		Text:      "reply body",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Errorf("Send returned empty Message-ID")
	}
	reqs := smtp.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("smtp got %d requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.From != cfg.FromAddress {
		t.Errorf("From = %q, want %q", got.From, cfg.FromAddress)
	}
	if len(got.To) != 1 || got.To[0] != "alice@ext.test" {
		t.Errorf("To = %v, want [alice@ext.test]", got.To)
	}
	payload := string(got.Payload)
	if !strings.Contains(payload, "Subject: Re: Topic") {
		t.Errorf("payload missing subject:\n%s", payload)
	}
	if !strings.Contains(payload, "In-Reply-To: <thread-root>") {
		t.Errorf("payload missing In-Reply-To header:\n%s", payload)
	}
	if !strings.Contains(payload, "References: <thread-root>") {
		t.Errorf("payload missing References header:\n%s", payload)
	}
	if !strings.Contains(payload, "reply body") {
		t.Errorf("payload missing body:\n%s", payload)
	}
	cancel()
	<-errCh
}

func TestSend_ExplicitToAndSubjectOverrideSession(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{}
	cfg.SMTPSender = smtp
	ch, _ := New(cfg)
	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "new-thread-id",
		Text:      "hello",
		ChannelSpecific: map[string]string{
			"to":      "bob@manual.test",
			"subject": "Explicit Subject",
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Errorf("Send returned empty Message-ID")
	}
	reqs := smtp.snapshot()
	if len(reqs) != 1 || reqs[0].To[0] != "bob@manual.test" {
		t.Fatalf("To = %v, want [bob@manual.test]", reqs[0].To)
	}
	if !strings.Contains(string(reqs[0].Payload), "Subject: Explicit Subject") {
		t.Errorf("payload missing explicit subject:\n%s", string(reqs[0].Payload))
	}
}

func TestSend_PropagatesSMTPError(t *testing.T) {
	cfg := validConfig()
	smtp := &fakeSMTP{sendErr: errors.New("relay rejected")}
	cfg.SMTPSender = smtp
	ch, _ := New(cfg)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "x",
		Text:      "body",
		ChannelSpecific: map[string]string{
			"to":      "bob@test",
			"subject": "Subj",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "relay rejected") {
		t.Errorf("Send err = %v, want to wrap relay rejected", err)
	}
}

// ---- ListSessions + ResolveSpeaker ----

func TestListSessions_EmptyBeforeAnyEvent(t *testing.T) {
	ch, _ := New(validConfig())
	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("ListSessions = %d entries, want 0", len(sessions))
	}
}

func TestListSessions_PopulatedAfterPoll(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "200", Body: sampleEmail("root-A", "alice@ext.test", "Topic A", "body A")},
		{UID: "201", Body: sampleEmail("root-B", "bob@ext.test", "Topic B", "body B")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 2 })
	sessions, err := ch.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("ListSessions = %d entries, want 2", len(sessions))
	}
	// Newest-first sort — both messages share the same Date in the
	// sample fixture, so we just verify both sessions are present.
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s.ID] = true
		if s.ParticipantCount != 1 {
			t.Errorf("session %q ParticipantCount = %d, want 1", s.ID, s.ParticipantCount)
		}
	}
	if !seen["root-A"] || !seen["root-B"] {
		t.Errorf("sessions missing expected ids: %v", seen)
	}
	cancel()
	<-errCh
}

func TestListSessions_MultipleMessagesSameThreadCollapse(t *testing.T) {
	cfg := validConfig()
	imap := newFakeIMAP([]RawMessage{
		{UID: "300", Body: sampleEmail("root-X", "alice@ext.test", "Topic X", "first")},
	}, []RawMessage{
		{UID: "301", Body: sampleEmailThread("reply-X1", "bob@ext.test", "Re: Topic X", "second", "root-X", "root-X")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 2 })
	sessions, _ := ch.ListSessions(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 (same thread)", len(sessions))
	}
	if sessions[0].ParticipantCount != 2 {
		t.Errorf("ParticipantCount = %d, want 2 (alice + bob)", sessions[0].ParticipantCount)
	}
	cancel()
	<-errCh
}

func TestResolveSpeaker_EmptyAllowlistPassesThrough(t *testing.T) {
	ch, _ := New(validConfig())
	sp, err := ch.ResolveSpeaker(context.Background(), "alice@external.test")
	if err != nil {
		t.Fatalf("ResolveSpeaker: %v", err)
	}
	if sp.ID != "email:alice@external.test" {
		t.Errorf("Speaker.ID = %q", sp.ID)
	}
	if sp.ChannelHandle != "alice@external.test" {
		t.Errorf("Speaker.ChannelHandle = %q", sp.ChannelHandle)
	}
}

func TestResolveSpeaker_AllowlistRejectsUnknown(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"trusted.test"}
	ch, _ := New(cfg)
	_, err := ch.ResolveSpeaker(context.Background(), "stranger@evil.test")
	if !errors.Is(err, conversation.ErrSpeakerUnknown) {
		t.Errorf("err = %v, want ErrSpeakerUnknown", err)
	}
}

func TestResolveSpeaker_DomainMatchAdmits(t *testing.T) {
	cfg := validConfig()
	cfg.SenderAllowlist = []string{"trusted.test"}
	ch, _ := New(cfg)
	sp, err := ch.ResolveSpeaker(context.Background(), "alice@trusted.test")
	if err != nil {
		t.Fatalf("ResolveSpeaker: %v", err)
	}
	if sp.ID != "email:alice@trusted.test" {
		t.Errorf("Speaker.ID = %q", sp.ID)
	}
}

// ---- allowlist matrix ----

func TestSenderAllowlist_PermitAllOnEmpty(t *testing.T) {
	l := newSenderAllowlist(nil)
	if !l.allows("anything@anywhere") {
		t.Error("empty allowlist must permit all")
	}
}

func TestSenderAllowlist_OnlyWhitespaceIsEmpty(t *testing.T) {
	l := newSenderAllowlist([]string{"  ", "\t", ""})
	if !l.permitAll {
		t.Error("whitespace-only entries should collapse to permit-all")
	}
}

func TestSenderAllowlist_RejectsMalformedAddress(t *testing.T) {
	l := newSenderAllowlist([]string{"example.com"})
	if l.allows("not-an-email") {
		t.Error("malformed address must not match a domain entry")
	}
}

func TestSenderAllowlist_EmptyFromRejected(t *testing.T) {
	l := newSenderAllowlist([]string{"example.com"})
	if l.allows("") {
		t.Error("empty From must not pass a non-empty allowlist")
	}
}

// ---- slice-2 attachment end-to-end ----

// sampleEmailWithAttachment builds a multipart/mixed email with a
// text body and one named attachment. Used to exercise the
// attachment-persistence path through Channel.handleRawMessage.
func sampleEmailWithAttachment(messageID, from, subject, body, filename, attachContent string) []byte {
	hdrs := "From: " + from + "\r\n" +
		"To: vornik@test\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
		"Message-ID: <" + messageID + ">\r\n" +
		`Content-Type: multipart/mixed; boundary="MIXED"` + "\r\n" +
		"\r\n" +
		"--MIXED\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" +
		body + "\r\n" +
		"--MIXED\r\n" +
		`Content-Type: application/pdf; name="` + filename + `"` + "\r\n" +
		`Content-Disposition: attachment; filename="` + filename + `"` + "\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		attachContent + "\r\n" +
		"--MIXED--\r\n"
	return []byte(hdrs)
}

func TestChannel_PersistsAttachmentsOnInbound(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"

	imap := newFakeIMAP([]RawMessage{
		{UID: "9001", Body: sampleEmailWithAttachment("att-msg-1", "alice@ext.test", "with attachment",
			"Please review.", "report.pdf", "PDFBYTES")},
	})
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if len(got.Attachments) != 1 {
		t.Fatalf("ChannelMessage.Attachments = %d, want 1", len(got.Attachments))
	}
	att := got.Attachments[0]
	if att.Name != "report.pdf" {
		t.Errorf("Attachment.Name = %q, want report.pdf", att.Name)
	}
	if att.MimeType != "application/pdf" {
		t.Errorf("Attachment.MimeType = %q", att.MimeType)
	}
	if att.SizeBytes != int64(len("PDFBYTES")) {
		t.Errorf("Attachment.SizeBytes = %d", att.SizeBytes)
	}
	if att.ChannelRef == "" {
		t.Errorf("Attachment.ChannelRef empty (must be storage path)")
	}
	if att.ArtifactID == "" {
		t.Errorf("Attachment.ArtifactID empty — dispatcher needs this to call read_artifact(id=...)")
	}
	if len(repo.created) != 1 {
		t.Errorf("artifact repo Create calls = %d, want 1", len(repo.created))
	}
	if repo.created[0].ID != att.ArtifactID {
		t.Errorf("Attachment.ArtifactID = %q, want repo ID %q", att.ArtifactID, repo.created[0].ID)
	}
	if got.ChannelSpecific["attachment_count"] != "1" {
		t.Errorf("ChannelSpecific[attachment_count] = %q", got.ChannelSpecific["attachment_count"])
	}
	cancel()
	<-errCh
}

// fakeAutoExtractor is a programmable AttachmentAutoExtractor for
// the auto-trigger tests. Records every call so the assertions can
// verify the channel forwarded the right artifact + MIME + name.
type fakeAutoExtractor struct {
	mu       sync.Mutex
	calls    []AutoExtractRequest
	response *AttachmentExtraction
	err      error
}

func (f *fakeAutoExtractor) AutoExtract(_ context.Context, in AutoExtractRequest) (*AttachmentExtraction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.response, f.err
}

func (f *fakeAutoExtractor) snapshot() []AutoExtractRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AutoExtractRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestChannel_AutoExtractsAttachmentOnInbound pins the wiring
// between the email channel and the document-extraction pipeline.
// When an attachment lands, the channel must call the
// AutoExtractor exactly once with the persisted artifact's
// identifiers, and fold the returned summary into the outgoing
// ChannelMessage.Attachments[i].Extraction. The dispatcher LLM
// reads this trailer to decide "this is already in memory — no
// task needed."
func TestChannel_AutoExtractsAttachmentOnInbound(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"
	cfg.AutoExtractor = &fakeAutoExtractor{
		response: &AttachmentExtraction{
			ExtractedDocumentID: "extdoc_abc",
			Title:               "Test Book",
			Author:              "Test Author",
			SectionCount:        18,
			ChunksIngested:      412,
		},
	}

	imap := newFakeIMAP([]RawMessage{
		{UID: "9100", Body: sampleEmailWithAttachment("att-extr-1", "alice@ext.test", "ingestable",
			"please add to memory", "book.epub", "EPUBBYTES")},
	})
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if len(got.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(got.Attachments))
	}
	att := got.Attachments[0]
	if att.Extraction == nil {
		t.Fatal("Attachment.Extraction is nil; auto-extract summary did not flow")
	}
	if att.Extraction.ExtractedDocumentID != "extdoc_abc" {
		t.Errorf("ExtractedDocumentID = %q; want extdoc_abc", att.Extraction.ExtractedDocumentID)
	}
	if att.Extraction.Title != "Test Book" || att.Extraction.Author != "Test Author" {
		t.Errorf("title/author lost in handoff: %q / %q",
			att.Extraction.Title, att.Extraction.Author)
	}
	if att.Extraction.SectionCount != 18 || att.Extraction.ChunksIngested != 412 {
		t.Errorf("section_count=%d chunks_ingested=%d",
			att.Extraction.SectionCount, att.Extraction.ChunksIngested)
	}

	calls := cfg.AutoExtractor.(*fakeAutoExtractor).snapshot()
	if len(calls) != 1 {
		t.Fatalf("AutoExtractor called %d times; want 1", len(calls))
	}
	if calls[0].ProjectID != "proj-1" {
		t.Errorf("call.ProjectID = %q", calls[0].ProjectID)
	}
	if calls[0].Name != "book.epub" {
		t.Errorf("call.Name = %q", calls[0].Name)
	}
	if calls[0].StoragePath == "" || calls[0].ArtifactID == "" {
		t.Errorf("call missing StoragePath/ArtifactID: %+v", calls[0])
	}

	cancel()
	<-errCh
}

// TestChannel_AutoExtractFailureDoesNotBlockDelivery — extraction
// is best-effort: when AutoExtractor returns an error, the
// attachment still flows to the dispatcher (just without an
// Extraction summary). Mirrors the
// "persistence-failure-degrades-to-body-only" posture above.
func TestChannel_AutoExtractFailureDoesNotBlockDelivery(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"
	cfg.AutoExtractor = &fakeAutoExtractor{err: errors.New("extractor crashed")}

	imap := newFakeIMAP([]RawMessage{
		{UID: "9101", Body: sampleEmailWithAttachment("att-extr-2", "alice@ext.test", "broken",
			"body", "broken.epub", "JUNK")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if len(got.Attachments) != 1 {
		t.Fatalf("Attachments = %d", len(got.Attachments))
	}
	if got.Attachments[0].Extraction != nil {
		t.Errorf("Extraction must be nil on extractor error; got %+v", got.Attachments[0].Extraction)
	}

	cancel()
	<-errCh
}

// TestChannel_AutoExtractNilSummaryIsSilentSkip — when the
// AutoExtractor returns (nil, nil) it means "no registered
// extractor for this MIME". Channel must NOT log an error and
// must NOT populate Extraction; the attachment flows verbatim
// to the dispatcher.
func TestChannel_AutoExtractNilSummaryIsSilentSkip(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"
	cfg.AutoExtractor = &fakeAutoExtractor{} // nil response, nil error

	imap := newFakeIMAP([]RawMessage{
		{UID: "9102", Body: sampleEmailWithAttachment("att-extr-3", "alice@ext.test", "unknown",
			"body", "blob.bin", "BYTES")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if got.Attachments[0].Extraction != nil {
		t.Errorf("Extraction must remain nil for unsupported MIME; got %+v", got.Attachments[0].Extraction)
	}
	cancel()
	<-errCh
}

func TestChannel_RejectsAttachmentsOverCap(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"
	cfg.AttachmentSizeCapBytes = 4 // 4-byte cap — easy to bust

	imap := newFakeIMAP([]RawMessage{
		{UID: "9002", Body: sampleEmailWithAttachment("att-msg-2", "alice@ext.test", "huge",
			"body", "big.pdf", "MORE-THAN-FOUR-BYTES")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// The cap-busting message must be marked seen + dropped.
	waitFor(t, func() bool { return len(imap.snapshotMarkSeen()) == 1 })
	if got := len(recv.snapshot()); got != 0 {
		t.Errorf("Receiver got %d, want 0 (cap-busting message must be dropped)", got)
	}
	cancel()
	<-errCh
}

func TestChannel_DegradesGracefullyWhenAttachmentPersistenceFails(t *testing.T) {
	cfg := validConfig()
	dir := t.TempDir()
	repo := &fakeArtifactRepo{createErr: errors.New("DB explosion")}
	cfg.ArtifactRepo = repo
	cfg.AttachmentStoreDir = dir
	cfg.AttachmentProjectID = "proj-1"

	imap := newFakeIMAP([]RawMessage{
		{UID: "9003", Body: sampleEmailWithAttachment("att-msg-3", "alice@ext.test", "broken",
			"body", "x.pdf", "BYTES")},
	})
	cfg.IMAPClient = imap
	ch, _ := New(cfg)
	recv := &captureReceiver{}
	cancel, errCh := startChannel(t, ch, recv)
	defer cancel()

	// Even with persistence failing, the body should still deliver.
	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]
	if !strings.Contains(got.Text, "body") {
		t.Errorf("text body lost on attachment failure: %q", got.Text)
	}
	if len(got.Attachments) != 0 {
		t.Errorf("Attachments should be empty when persistence fails, got %d", len(got.Attachments))
	}
	// attachment_count metadata is still recorded so operators can
	// correlate with the warn log.
	if got.ChannelSpecific["attachment_count"] != "1" {
		t.Errorf("ChannelSpecific[attachment_count] = %q (must record even on persist fail)", got.ChannelSpecific["attachment_count"])
	}
	cancel()
	<-errCh
}
