package email

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// newTestChannelForInbound builds a Channel wired only with what
// buildChannelMessage / SelfIdentity read.
func newTestChannelForInbound(t *testing.T, from string) *Channel {
	t.Helper()
	ch, err := New(Config{
		IMAPHost:     "imap.test",
		IMAPUsername: "u",
		IMAPPassword: "p",
		IMAPClient:   newFakeIMAP(),
		FromAddress:  from,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// Regression: incident 2026-07-28. The quoted trailer of the bot's own prior
// reply rode along in the user turn, so the lead attributed its own words to a
// third party. buildChannelMessage must hand the dispatcher only the new text.
func TestBuildChannelMessageStripsQuotedTrailer(t *testing.T) {
	ch := newTestChannelForInbound(t, "bot@vornik.io")
	parsed := ParsedMessage{
		From:      "janka@example.com",
		MessageID: "<m1@example.com>",
		Subject:   "Re: add these books to rag",
		Date:      time.Now(),
		Body: "4\n\nOn Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\n" +
			"> You tried uploading an epub, which I don't know how to process.\n> 1. ...\n",
	}

	msg := ch.buildChannelMessage(parsed, "uid-1", nil, nil)

	if msg.Text != "4" {
		t.Errorf("Text = %q; want %q (quoted trailer must be stripped)", msg.Text, "4")
	}
	if strings.Contains(msg.Text, "bot@vornik.io") {
		t.Error("Text still carries the quoted trailer naming the bot's own address")
	}
}

// The subject is the only place a call-to-action lives when the body is empty
// ("add these books to rag" in the subject, books attached, empty body). It
// must survive onto the message so the dispatcher can surface it.
func TestBuildChannelMessageCarriesSubject(t *testing.T) {
	ch := newTestChannelForInbound(t, "bot@vornik.io")
	parsed := ParsedMessage{
		From:      "janka@example.com",
		MessageID: "<m2@example.com>",
		Subject:   "add these books to rag",
		Body:      "",
	}

	msg := ch.buildChannelMessage(parsed, "uid-2", nil, nil)

	if got := msg.ChannelSpecific[conversation.ChannelSpecificSubject]; got != "add these books to rag" {
		t.Errorf("ChannelSpecific[subject] = %q; want the subject line", got)
	}
}

// Multi-party threads must stay attributable: the sender rides along so the
// dispatcher can stamp "From:" on the user turn.
func TestBuildChannelMessageCarriesSender(t *testing.T) {
	ch := newTestChannelForInbound(t, "bot@vornik.io")
	parsed := ParsedMessage{
		From:      "janka@example.com",
		MessageID: "<m3@example.com>",
		Body:      "I disagree with Vadim on the second point.",
	}

	msg := ch.buildChannelMessage(parsed, "uid-3", nil, nil)

	if got := msg.ChannelSpecific[conversation.ChannelSpecificSender]; got != "janka@example.com" {
		t.Errorf("ChannelSpecific[sender] = %q; want the From address", got)
	}
}

// SelfIdentity lets the dispatcher tell the LLM which address is its own, so a
// quoted block that survives stripping is not mistaken for a third party.
func TestChannelSelfIdentity(t *testing.T) {
	ch := newTestChannelForInbound(t, "bot@vornik.io")
	if got := ch.SelfIdentity(); got != "bot@vornik.io" {
		t.Errorf("SelfIdentity() = %q; want %q", got, "bot@vornik.io")
	}
}

func TestChannelSelfIdentityEmptyWhenUnconfigured(t *testing.T) {
	ch := newTestChannelForInbound(t, "")
	if got := ch.SelfIdentity(); got != "" {
		t.Errorf("SelfIdentity() = %q; want empty when FromAddress is unset", got)
	}
}

// End-to-end through the real poll loop: the exact incident shape. A terse
// reply picking option "4", with the bot's own prior reply quoted underneath by
// the mail client. The receiver must see "4" and nothing about the bot's
// address, while the subject still rides along in ChannelSpecific.
func TestPollerStripsQuotedTrailerEndToEnd(t *testing.T) {
	cfg := validConfig()
	cfg.FromAddress = "bot@vornik.io"
	body := "4\r\n\r\nOn Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\r\n" +
		"> You tried uploading an epub, which I don't know how to process.\r\n" +
		"> Here is what I can do instead:\r\n> 1. Summarise it\r\n> 4. Add it to RAG\r\n"
	imap := newFakeIMAP([]RawMessage{
		{UID: "1", Body: sampleEmail("thread@b", "janka@external.test", "Re: add these books to rag", body)},
	})
	cfg.IMAPClient = imap
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recv := &captureReceiver{}
	cancel, _ := startChannel(t, ch, recv)
	defer cancel()

	waitFor(t, func() bool { return len(recv.snapshot()) == 1 })
	got := recv.snapshot()[0]

	if strings.TrimSpace(got.Text) != "4" {
		t.Errorf("Text = %q; want just %q — the quoted trailer must not reach the LLM", got.Text, "4")
	}
	if strings.Contains(got.Text, "bot@vornik.io") {
		t.Error("Text carries the bot's own address from the quoted trailer")
	}
	if strings.Contains(got.Text, "epub") {
		t.Error("Text carries the bot's own quoted prior reply")
	}
	if got.ChannelSpecific["subject"] != "Re: add these books to rag" {
		t.Errorf("subject = %q; want it preserved for the dispatcher", got.ChannelSpecific["subject"])
	}
}
