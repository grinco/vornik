package email

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// captureReceiverForFollowup records every synthetic resume turn
// the channel feeds back to the dispatcher. Mirrors
// captureReceiver in channel_test.go but kept local so the
// follow-up tests can stay focused on the resume path.
type captureReceiverForFollowup struct {
	mu       sync.Mutex
	received []conversation.ChannelMessage
}

func (c *captureReceiverForFollowup) Receive(_ context.Context, msg conversation.ChannelMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, msg)
	return nil
}

func (c *captureReceiverForFollowup) snapshot() []conversation.ChannelMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]conversation.ChannelMessage(nil), c.received...)
}

// newTestChannelForFollowup builds a minimal Channel suitable for
// the follow-up tests (no IMAP/SMTP wiring; we only exercise the
// follow-up state + NotifyTaskCompleted path). Logger uses Nop so
// the test output stays clean.
func newTestChannelForFollowup() *Channel {
	return &Channel{
		logger:    zerolog.Nop(),
		followups: newFollowupStore(),
	}
}

// TestRegisterFollowup_RecordsThenClaim — register, claim, second
// claim must return ok=false (idempotency: a duplicate
// NotifyTaskCompleted can't fire the resume twice).
func TestRegisterFollowup_RecordsThenClaim(t *testing.T) {
	ch := newTestChannelForFollowup()
	ch.RegisterFollowup("session-1", "task-1", "proj-1")

	entry, ok := ch.followups.claim("task-1")
	if !ok {
		t.Fatal("first claim should succeed")
	}
	if entry.SessionID != "session-1" || entry.ProjectID != "proj-1" {
		t.Errorf("entry = %+v", entry)
	}
	if _, ok := ch.followups.claim("task-1"); ok {
		t.Error("second claim must return ok=false (already claimed)")
	}
}

// TestRegisterFollowup_GuardsEmpty — empty taskID or sessionID
// must be a no-op so a misuse in the dispatcher tool doesn't
// pollute the map with nonsense keys.
func TestRegisterFollowup_GuardsEmpty(t *testing.T) {
	ch := newTestChannelForFollowup()
	ch.RegisterFollowup("", "task-1", "proj-1")
	ch.RegisterFollowup("session-1", "", "proj-1")

	if _, ok := ch.followups.claim("task-1"); ok {
		t.Error("empty sessionID should not record")
	}
	if _, ok := ch.followups.claim(""); ok {
		t.Error("empty taskID should not record")
	}
}

// TestNotifyTaskCompleted_NotRegistered — unknown taskID is a
// no-op (the executor's CompletionNotifier fan-out hits every
// channel; non-matching ones must silently ignore so cross-
// channel leaks are impossible).
func TestNotifyTaskCompleted_NotRegistered(t *testing.T) {
	ch := newTestChannelForFollowup()
	recv := &captureReceiverForFollowup{}
	ch.recv = recv

	task := &persistence.Task{ID: "task-unknown", Status: persistence.TaskStatusCompleted}
	ch.NotifyTaskCompleted(context.Background(), task, true, "")

	if got := recv.snapshot(); len(got) != 0 {
		t.Errorf("unregistered task triggered Receive: %+v", got)
	}
}

// TestNotifyTaskCompleted_FiresResume — registered task → claim
// + synthetic resume turn delivered to the bound receiver. Pins
// the resume contract: SessionID threaded through, Text contains
// the task status + message, Source set to the channel name.
func TestNotifyTaskCompleted_FiresResume(t *testing.T) {
	ch := newTestChannelForFollowup()
	recv := &captureReceiverForFollowup{}
	ch.recv = recv
	ch.RegisterFollowup("thread-root-msgid", "task-99", "assistant")

	task := &persistence.Task{
		ID:        "task-99",
		ProjectID: "assistant",
		Status:    persistence.TaskStatusCompleted,
		CreatedAt: time.Now(),
	}
	ch.NotifyTaskCompleted(context.Background(), task, true, "Wrote summary.md")

	got := recv.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 receive, got %d", len(got))
	}
	if got[0].SessionID != "thread-root-msgid" {
		t.Errorf("SessionID = %q, want thread-root-msgid", got[0].SessionID)
	}
	if got[0].Source != channelName {
		t.Errorf("Source = %q, want %q", got[0].Source, channelName)
	}
	if !contains(got[0].Text, "task-99") {
		t.Errorf("Text missing task ID: %q", got[0].Text)
	}
	if !contains(got[0].Text, "Wrote summary.md") {
		t.Errorf("Text missing message: %q", got[0].Text)
	}
}

// TestNotifyTaskCompleted_NotificationStatusDerivedFromSuccess —
// the email synthetic turn must derive "completed" from the
// `success` bool, not from task.Status. Pre-fix a stale LEASED on
// the in-memory Task rendered as "reached terminal status: LEASED"
// (see 2026-05-21 telegram incident — same code path used to live
// here).
func TestNotifyTaskCompleted_NotificationStatusDerivedFromSuccess(t *testing.T) {
	ch := newTestChannelForFollowup()
	recv := &captureReceiverForFollowup{}
	ch.recv = recv
	ch.RegisterFollowup("thread-z", "task-stale", "p1")

	// Deliberately stale Status="LEASED" on the in-memory task.
	task := &persistence.Task{ID: "task-stale", Status: persistence.TaskStatusLeased}
	ch.NotifyTaskCompleted(context.Background(), task, true, "ok")

	got := recv.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 receive, got %d", len(got))
	}
	if !contains(got[0].Text, "completed successfully") {
		t.Errorf("expected literal 'completed successfully': %q", got[0].Text)
	}
	if contains(got[0].Text, "LEASED") {
		t.Errorf("stale task.Status leaked into notification: %q", got[0].Text)
	}
	if contains(got[0].Text, "terminal status") {
		t.Errorf("legacy 'terminal status' phrasing still present: %q", got[0].Text)
	}
}

// TestNotifyTaskCompleted_FailureSurfacesError — failed tasks
// must carry the LastError into the resume text so the LLM has
// something useful to report to the operator.
func TestNotifyTaskCompleted_FailureSurfacesError(t *testing.T) {
	ch := newTestChannelForFollowup()
	recv := &captureReceiverForFollowup{}
	ch.recv = recv
	ch.RegisterFollowup("thread-x", "task-fail", "proj-1")

	errMsg := "container exited with code 137 (OOM)"
	task := &persistence.Task{
		ID:        "task-fail",
		Status:    persistence.TaskStatusFailed,
		LastError: &errMsg,
	}
	ch.NotifyTaskCompleted(context.Background(), task, false, "")

	got := recv.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 receive, got %d", len(got))
	}
	if !contains(got[0].Text, "did NOT complete successfully") {
		t.Errorf("Text should mark failure: %q", got[0].Text)
	}
	if !contains(got[0].Text, "OOM") {
		t.Errorf("Text should surface LastError: %q", got[0].Text)
	}
}

// TestNotifyTaskCompleted_ReceiverUnbound — channel.Start hasn't
// been called yet so recv is nil. The notifier logs and drops
// silently rather than panicking.
func TestNotifyTaskCompleted_ReceiverUnbound(t *testing.T) {
	ch := newTestChannelForFollowup()
	ch.RegisterFollowup("thread-y", "task-1", "proj-1")
	task := &persistence.Task{ID: "task-1", Status: persistence.TaskStatusCompleted}
	// Must not panic.
	ch.NotifyTaskCompleted(context.Background(), task, true, "")
}

// TestNotifyTaskCompleted_NilChannel — defensive nil-receiver
// guard so the executor's fan-out doesn't crash if a channel
// slot is somehow nil.
func TestNotifyTaskCompleted_NilChannel(t *testing.T) {
	var ch *Channel
	task := &persistence.Task{ID: "x", Status: persistence.TaskStatusCompleted}
	ch.NotifyTaskCompleted(context.Background(), task, true, "")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
