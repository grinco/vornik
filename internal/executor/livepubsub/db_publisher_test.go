package livepubsub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeLiveEventRepo is an in-memory ExecutionLiveEventRepository
// mirroring the Postgres semantics for tests.
type fakeLiveEventRepo struct {
	mu          sync.Mutex
	rows        map[string][]*persistence.ExecutionLiveEvent
	appendErr   error
	listErr     error
	appendCalls int
}

func newFakeLiveEventRepo() *fakeLiveEventRepo {
	return &fakeLiveEventRepo{rows: map[string][]*persistence.ExecutionLiveEvent{}}
}

func (f *fakeLiveEventRepo) Append(_ context.Context, executionID, kind string, payload []byte) (int64, error) {
	if f.appendErr != nil {
		return 0, f.appendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls++
	seq := int64(0)
	if rs := f.rows[executionID]; len(rs) > 0 {
		seq = rs[len(rs)-1].Seq + 1
	}
	row := &persistence.ExecutionLiveEvent{
		ID:          int64(f.appendCalls),
		ExecutionID: executionID,
		Seq:         seq,
		Kind:        kind,
		Payload:     append([]byte(nil), payload...),
		CreatedAt:   time.Now().UTC(),
	}
	f.rows[executionID] = append(f.rows[executionID], row)
	return seq, nil
}

func (f *fakeLiveEventRepo) ListSince(_ context.Context, executionID string, fromSeq int64, limit int) ([]*persistence.ExecutionLiveEvent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.ExecutionLiveEvent
	for _, row := range f.rows[executionID] {
		if row.Seq >= fromSeq {
			cp := *row
			cp.Payload = append([]byte(nil), row.Payload...)
			out = append(out, &cp)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeLiveEventRepo) LatestSeq(_ context.Context, executionID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rs := f.rows[executionID]; len(rs) > 0 {
		return rs[len(rs)-1].Seq, nil
	}
	return -1, nil
}

func (f *fakeLiveEventRepo) DeleteOlderThan(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

// fakeNotifier records every Notify call. Used to assert the
// cross-replica publish-side fanout fired.
type fakeNotifier struct {
	mu     sync.Mutex
	calls  []struct{ Channel, Payload string }
	notify chan struct{}
	err    error
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{notify: make(chan struct{}, 32)}
}

func (n *fakeNotifier) Notify(_ context.Context, channel, payload string) error {
	if n.err != nil {
		return n.err
	}
	n.mu.Lock()
	n.calls = append(n.calls, struct{ Channel, Payload string }{channel, payload})
	n.mu.Unlock()
	select {
	case n.notify <- struct{}{}:
	default:
	}
	return nil
}

// fakeListener feeds Notifications onto its channel — tests
// simulate a remote replica's NOTIFY by pushing to the channel.
type fakeListener struct {
	ch chan Notification
}

func newFakeListener() *fakeListener {
	return &fakeListener{ch: make(chan Notification, 32)}
}

func (l *fakeListener) Start(_ context.Context, _ string) (<-chan Notification, error) {
	return l.ch, nil
}

// TestDBBackedPublisher_LocalPublishPersistsAndFansOut: the
// happy-path single-replica behaviour. Publish writes to DB,
// fires NOTIFY, AND delivers locally to a subscriber.
func TestDBBackedPublisher_LocalPublishPersistsAndFansOut(t *testing.T) {
	repo := newFakeLiveEventRepo()
	notif := newFakeNotifier()
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo:     repo,
		Notifier: notif,
		NodeID:   "node-a",
		Logger:   zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	ch, cancel, err := pub.Subscribe("exec-1", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	seq := pub.Publish(context.Background(), "exec-1", "step_started", StepStartedPayload{StepID: "s1"})
	if seq != 0 {
		t.Errorf("first seq = %d, want 0", seq)
	}

	select {
	case evt := <-ch:
		if evt.Kind != "step_started" || evt.Seq != 0 {
			t.Errorf("event mismatch: %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber didn't receive event")
	}

	// DB row persisted.
	if repo.appendCalls != 1 {
		t.Errorf("Append called %d times, want 1", repo.appendCalls)
	}
	// NOTIFY fired with the right payload shape.
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.calls) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notif.calls))
	}
	if !strings.HasPrefix(notif.calls[0].Payload, "exec-1|0|node-a") {
		t.Errorf("payload format unexpected: %q", notif.calls[0].Payload)
	}
}

// TestDBBackedPublisher_RemoteNotifyIngestsIntoLocalRing: the
// headline cross-replica contract. A NOTIFY arriving from
// another replica (different nodeID) should trigger a DB fetch
// + local fanout to subscribers of this replica.
func TestDBBackedPublisher_RemoteNotifyIngestsIntoLocalRing(t *testing.T) {
	repo := newFakeLiveEventRepo()
	notif := newFakeNotifier()
	listener := newFakeListener()

	// Pre-seed the DB as if another replica wrote the event.
	pl, _ := json.Marshal(StepStartedPayload{StepID: "from-replica-b"})
	if _, err := repo.Append(context.Background(), "exec-cross", "step_started", pl); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo:     repo,
		Notifier: notif,
		Listener: listener,
		NodeID:   "node-a",
		Logger:   zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	ch, cancel, err := pub.Subscribe("exec-cross", 1) // start AFTER the seeded row
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Drain any replay frames the seed produced.
	drain(ch)

	// Simulate a NOTIFY from replica B about a NEW event it just persisted.
	pl2, _ := json.Marshal(StepStartedPayload{StepID: "second-from-b"})
	seqB, _ := repo.Append(context.Background(), "exec-cross", "step_started", pl2)
	listener.ch <- Notification{Channel: NotifyChannel, Payload: "exec-cross|" + itoa(seqB) + "|node-b"}

	// We should receive the remote event locally.
	select {
	case evt := <-ch:
		if evt.Seq != seqB {
			t.Errorf("seq = %d, want %d", evt.Seq, seqB)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber didn't receive remote event")
	}
}

// TestDBBackedPublisher_SelfNotificationIsSkipped: an emitting
// replica's own NOTIFY shouldn't trigger double-delivery on
// itself. The local Publish already fanned out via IngestRemote.
func TestDBBackedPublisher_SelfNotificationIsSkipped(t *testing.T) {
	repo := newFakeLiveEventRepo()
	notif := newFakeNotifier()
	listener := newFakeListener()
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo:     repo,
		Notifier: notif,
		Listener: listener,
		NodeID:   "node-self",
		Logger:   zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	ch, cancel, err := pub.Subscribe("exec-loop", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	pub.Publish(context.Background(), "exec-loop", "step_started", StepStartedPayload{StepID: "x"})

	// First receive: the local fanout.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("missed local event")
	}

	// Simulate the listener seeing our own NOTIFY echoed back.
	listener.ch <- Notification{Channel: NotifyChannel, Payload: "exec-loop|0|node-self"}

	// No second event should arrive — the self-skip kicked in.
	select {
	case evt := <-ch:
		t.Errorf("self-notification re-delivered: %+v", evt)
	case <-time.After(200 * time.Millisecond):
		// Expected.
	}
}

// TestDBBackedPublisher_DBFailureFallsBackToInProcess: Append
// returning an error must NOT break the local stream. The
// subscriber still receives the event via the in-process
// fallback path.
func TestDBBackedPublisher_DBFailureFallsBackToInProcess(t *testing.T) {
	repo := newFakeLiveEventRepo()
	repo.appendErr = errors.New("connection refused")
	notif := newFakeNotifier()
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo:     repo,
		Notifier: notif,
		NodeID:   "node-a",
		Logger:   zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	ch, cancel, err := pub.Subscribe("exec-degraded", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	pub.Publish(context.Background(), "exec-degraded", "step_started", StepStartedPayload{StepID: "x"})

	select {
	case evt := <-ch:
		if evt.Kind != "step_started" {
			t.Errorf("event mismatch: %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("DB failure broke the local stream — fallback didn't fire")
	}

	// Notify must not have fired (no seq to broadcast).
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.calls) != 0 {
		t.Errorf("NOTIFY fired with no successful DB write")
	}
}

// TestDBBackedPublisher_SubscribeReplaysFromDBWhenRingEmpty:
// late-joining subscriber on a replica that wasn't running the
// execution. The in-memory ring is empty; replay must come from
// the DB.
func TestDBBackedPublisher_SubscribeReplaysFromDBWhenRingEmpty(t *testing.T) {
	repo := newFakeLiveEventRepo()
	// Pre-seed several events as if produced by another replica.
	for i := 0; i < 3; i++ {
		pl, _ := json.Marshal(StepStartedPayload{StepID: "step-" + itoa(int64(i))})
		_, _ = repo.Append(context.Background(), "exec-replay", "step_started", pl)
	}
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Repo:   repo,
		NodeID: "node-late",
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	ch, cancel, err := pub.Subscribe("exec-replay", 1) // want seqs 1, 2
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	got := drainNonBlocking(ch, 200*time.Millisecond, 4)
	if len(got) < 2 {
		t.Fatalf("replay returned %d events, want >= 2: %+v", len(got), got)
	}
	// Filter out potential ReplayGapMarker; care about the real events.
	seqs := map[int64]bool{}
	for _, e := range got {
		if e.Kind == KindReplayGap {
			continue
		}
		seqs[e.Seq] = true
	}
	if !seqs[1] || !seqs[2] {
		t.Errorf("expected seqs 1,2 from replay; got %v", seqs)
	}
}

// TestParseNotifyPayload_RoundTrip pins the wire format. A drift
// here would silently break cross-replica delivery.
func TestParseNotifyPayload_RoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		execID  string
		seq     int64
		nodeID  string
		wantErr bool
	}{
		{"ok", "exec-1|42|node-b", "exec-1", 42, "node-b", false},
		{"missing-parts", "exec-1|42", "", 0, "", true},
		{"bad-seq", "exec-1|notanumber|node-b", "", 0, "", true},
		{"empty-payload", "", "", 0, "", true},
		{"id-with-dashes", "task_20260523_abc|7|hostname:1234:abc", "task_20260523_abc", 7, "hostname:1234:abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execID, seq, nodeID, ok := parseNotifyPayload(tc.input)
			if tc.wantErr {
				if ok {
					t.Errorf("expected parse failure for %q", tc.input)
				}
				return
			}
			if !ok || execID != tc.execID || seq != tc.seq || nodeID != tc.nodeID {
				t.Errorf("parseNotifyPayload(%q) = (%q, %d, %q, %v); want (%q, %d, %q, true)",
					tc.input, execID, seq, nodeID, ok, tc.execID, tc.seq, tc.nodeID)
			}
		})
	}
}

// TestDBBackedPublisher_NilRepoErrors guards the constructor's
// preconditions. Passing nil Repo would silently break later.
func TestDBBackedPublisher_NilRepoErrors(t *testing.T) {
	_, _, err := NewDBBacked(context.Background(), NewDBBackedConfig{Repo: nil})
	if err == nil {
		t.Errorf("expected error on nil repo")
	}
}

// TestInProcessPublisher_IngestRemoteBumpsNextSeq: the
// IngestRemote contract — receiving a remote event with seq=5
// must NOT let a subsequent local Publish() reuse seq <=5.
// Without this guard, two replicas could allocate the same local
// seq for different events.
func TestInProcessPublisher_IngestRemoteBumpsNextSeq(t *testing.T) {
	p := &inProcessPublisher{streams: map[string]*stream{}, ringSize: 50}
	p.IngestRemote(LiveEvent{ExecutionID: "exec-bump", Seq: 9, Kind: "step_started", Timestamp: time.Now()})
	// Now allocate a local seq via Publish.
	got := p.Publish(context.Background(), "exec-bump", "step_completed", StepCompletedPayload{StepID: "x"})
	if got <= 9 {
		t.Errorf("local Publish allocated seq=%d after IngestRemote(seq=9); should be > 9", got)
	}
}

// helpers ------------------------------------------------------

func drain(ch <-chan LiveEvent) {
	for {
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func drainNonBlocking(ch <-chan LiveEvent, timeout time.Duration, max int) []LiveEvent {
	var out []LiveEvent
	deadline := time.Now().Add(timeout)
	for {
		if len(out) >= max {
			return out
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		select {
		case evt := <-ch:
			out = append(out, evt)
		case <-time.After(remaining):
			return out
		}
	}
}

func itoa(n int64) string {
	// strconv.FormatInt without importing; the test file already
	// uses small numbers and prefers no extra imports.
	return strings.TrimSpace(formatInt(n))
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 0, 16)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
