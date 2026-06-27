package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/registry"
)

// fakeChannelSender mocks the email.Channel surface the adapter
// consumes. Records Send calls so tests can pin the routing +
// envelope construction (To/Subject/Body translation into
// ChannelSpecific) without spinning up SMTP.
type fakeChannelSender struct {
	mu sync.Mutex

	calls       []conversation.ChannelMessage
	returnMsgID string
	returnErr   error
}

func (f *fakeChannelSender) Send(_ context.Context, msg conversation.ChannelMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, msg)
	if f.returnErr != nil {
		return "", f.returnErr
	}
	if f.returnMsgID != "" {
		return f.returnMsgID, nil
	}
	return "msg-001@vornik.local", nil
}

func (f *fakeChannelSender) snapshot() []conversation.ChannelMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]conversation.ChannelMessage(nil), f.calls...)
}

// TestNewEmailSender_NoChannels — empty channel slice → nil
// adapter so the dispatcher's send_email tool reports "not
// configured" rather than the adapter silently swallowing calls.
func TestNewEmailSender_NoChannels(t *testing.T) {
	if got := newEmailSender(nil, nil); got != nil {
		t.Errorf("nil channels: got %v, want nil", got)
	}
	if got := newEmailSender([]channelSendCloser{}, []*registry.Project{}); got != nil {
		t.Errorf("empty channels: got %v, want nil", got)
	}
}

// TestEmailSender_RoutesByProject — multi-project deployments must
// route SendEmail("proj-A", ...) to channel[A], not channel[B].
// Getting this wrong cross-leaks email addresses across tenants.
func TestEmailSender_RoutesByProject(t *testing.T) {
	chA := &fakeChannelSender{returnMsgID: "from-A"}
	chB := &fakeChannelSender{returnMsgID: "from-B"}
	projA := &registry.Project{ID: "proj-A"}
	projB := &registry.Project{ID: "proj-B"}

	sender := newEmailSender(
		[]channelSendCloser{chA, chB},
		[]*registry.Project{projA, projB},
	)
	if sender == nil {
		t.Fatal("adapter unexpectedly nil")
	}

	got, err := sender.SendEmail(context.Background(), "proj-B", dispatcher.EmailSendRequest{
		To: "x@example.com", Subject: "hi", Body: "hello",
	})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if got != "from-B" {
		t.Errorf("Message-ID = %q, want from-B (routing landed on wrong channel)", got)
	}
	if len(chA.snapshot()) != 0 {
		t.Error("channel A must not be called when projectID is proj-B")
	}
	if len(chB.snapshot()) != 1 {
		t.Errorf("channel B calls = %d, want 1", len(chB.snapshot()))
	}
}

// TestEmailSender_UnknownProject — caller passes a projectID that
// isn't wired. Adapter must error explicitly (not silently route
// to channel[0]) so the dispatcher surfaces a real reason.
func TestEmailSender_UnknownProject(t *testing.T) {
	ch := &fakeChannelSender{}
	sender := newEmailSender(
		[]channelSendCloser{ch},
		[]*registry.Project{{ID: "proj-A"}},
	)
	_, err := sender.SendEmail(context.Background(), "proj-NOPE", dispatcher.EmailSendRequest{
		To: "x@example.com", Subject: "hi", Body: "hello",
	})
	if err == nil {
		t.Fatal("expected error for unknown project; got nil")
	}
	if !strings.Contains(err.Error(), "proj-NOPE") {
		t.Errorf("error must name the missing project; got %q", err)
	}
	if len(ch.snapshot()) != 0 {
		t.Error("no channel should be called for unknown project")
	}
}

// TestEmailSender_BuildsChannelMessage — the Channel.Send contract
// takes a ChannelMessage with ChannelSpecific["to"] and ["subject"]
// (channel.go:730). The adapter must build that envelope correctly
// or Channel.Send will fall back to its session-lookup path and
// error with ErrUnknownSession on a fresh-compose call.
func TestEmailSender_BuildsChannelMessage(t *testing.T) {
	ch := &fakeChannelSender{returnMsgID: "envelope-test"}
	sender := newEmailSender(
		[]channelSendCloser{ch},
		[]*registry.Project{{ID: "proj-A"}},
	)
	_, err := sender.SendEmail(context.Background(), "proj-A", dispatcher.EmailSendRequest{
		To:      "bob@example.com",
		Subject: "Daily news",
		Body:    "Top stories:\n- A\n- B",
	})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	calls := ch.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Send calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.ChannelSpecific["to"] != "bob@example.com" {
		t.Errorf("ChannelSpecific[to] = %q, want bob@example.com", c.ChannelSpecific["to"])
	}
	if c.ChannelSpecific["subject"] != "Daily news" {
		t.Errorf("ChannelSpecific[subject] = %q", c.ChannelSpecific["subject"])
	}
	if !strings.Contains(c.Text, "Top stories") {
		t.Errorf("Body lost in translation; Text=%q", c.Text)
	}
	// Channel.Send routes by Source field; must be the email
	// channel name so the channel-receiver layer doesn't loop.
	if c.Source != "email" {
		t.Errorf("Source = %q, want email", c.Source)
	}
}

// TestEmailSender_ChannelError — Send returns a transport error;
// adapter must propagate verbatim so the LLM sees the SMTP reason
// (e.g. "550 alias not permitted") rather than a generic
// "send failed."
func TestEmailSender_ChannelError(t *testing.T) {
	ch := &fakeChannelSender{returnErr: errors.New("smtp: connection refused")}
	sender := newEmailSender(
		[]channelSendCloser{ch},
		[]*registry.Project{{ID: "proj-A"}},
	)
	_, err := sender.SendEmail(context.Background(), "proj-A", dispatcher.EmailSendRequest{
		To: "a@b.c", Subject: "x", Body: "y",
	})
	if err == nil {
		t.Fatal("expected propagated error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error must include underlying SMTP reason; got %q", err)
	}
}

// TestEmailSender_InReplyToPropagates — slice-1 threading: when
// the LLM provides in_reply_to, the adapter must stash it on
// ChannelSpecific so Channel.Send populates the In-Reply-To +
// References headers per RFC 5322.
func TestEmailSender_InReplyToPropagates(t *testing.T) {
	ch := &fakeChannelSender{}
	sender := newEmailSender(
		[]channelSendCloser{ch},
		[]*registry.Project{{ID: "proj-A"}},
	)
	_, err := sender.SendEmail(context.Background(), "proj-A", dispatcher.EmailSendRequest{
		To:        "a@b.c",
		Subject:   "Re: hi",
		Body:      "reply",
		InReplyTo: "original-msg@b.c",
	})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	calls := ch.snapshot()
	if len(calls) != 1 || calls[0].ChannelSpecific["in_reply_to"] != "original-msg@b.c" {
		t.Errorf("ChannelSpecific[in_reply_to] missing; got %+v", calls[0].ChannelSpecific)
	}
}

// TestEmailSender_MismatchedLengths — defensive guard: the slices
// are paired by index. If lengths diverge the adapter must still
// route correctly for projects up to min(len) and error cleanly
// for indices that have a nil channel or missing project.
func TestEmailSender_NilEntriesSkipped(t *testing.T) {
	chA := &fakeChannelSender{returnMsgID: "from-A"}
	sender := newEmailSender(
		[]channelSendCloser{nil, chA},
		[]*registry.Project{nil, {ID: "proj-A"}},
	)
	if sender == nil {
		t.Fatal("adapter unexpectedly nil")
	}
	// Existing project still works.
	got, err := sender.SendEmail(context.Background(), "proj-A", dispatcher.EmailSendRequest{
		To: "x@example.com", Subject: "y", Body: "z",
	})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if got != "from-A" {
		t.Errorf("Message-ID = %q", got)
	}
}

// channelSendCloser is the narrow seam the adapter consumes. Real
// production type is *email.Channel; tests inject fakeChannelSender.
// Defined here as a compile-time interface assertion so the test
// file documents the contract the adapter relies on.
var _ channelSendCloser = (*fakeChannelSender)(nil)
var _ channelSendCloser = (*email.Channel)(nil)
