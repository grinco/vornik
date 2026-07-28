package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/webchat"
)

// TestWebChatDisclosureIsRenderedNotJustRecorded pins the defect found by
// end-to-end testing against production on 2026-07-29.
//
// The disclosure reaches the web chat via Channel.Send, which for webchat
// buffers into channel.Sent(). ChatPostMessage rendered only
// store.History(...), so the notice was written to channel_disclosure_log as
// "served" while the human never saw it — a FALSE CONFORMITY RECORD, which is
// strictly worse than not disclosing at all: the evidence trail asserts an
// Art 50(1) disclosure that did not happen.
//
// Unit tests could not catch this. They asserted Channel.Send was called,
// which was true. The gap was between "Send was called" and "the human saw
// it", and only webchat has that gap because its Send is a buffer the handler
// ignored.
func TestWebChatDisclosureIsRenderedNotJustRecorded(t *testing.T) {
	svc := aidisclosure.New(aidisclosure.Config{}, nil)
	notice := svc.Notice().Text

	ch := webchat.New("proj", conversation.Speaker{ID: "web-chat:sess"})
	if _, err := ch.Send(t.Context(), conversation.ChannelMessage{Text: notice}); err != nil {
		t.Fatalf("Send disclosure: %v", err)
	}
	if _, err := ch.Send(t.Context(), conversation.ChannelMessage{Text: "the assistant reply"}); err != nil {
		t.Fatalf("Send reply: %v", err)
	}

	rendered := mergeDisclosureIntoHistory(nil, ch, svc)

	if len(rendered) == 0 {
		t.Fatal("no history rendered")
	}
	if !strings.Contains(rendered[0].Content, "interacting with an AI system") {
		t.Errorf("the Art 50 disclosure must be the FIRST rendered message; got %q", rendered[0].Content)
	}
}

// TestWebChatDisclosureNotDuplicatedWhenAbsent — on turns where the notice was
// not re-served (per-session cadence), nothing is prepended.
func TestWebChatDisclosureNotDuplicatedWhenAbsent(t *testing.T) {
	svc := aidisclosure.New(aidisclosure.Config{}, nil)
	ch := webchat.New("proj", conversation.Speaker{ID: "web-chat:sess"})
	if _, err := ch.Send(t.Context(), conversation.ChannelMessage{Text: "just a reply"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := mergeDisclosureIntoHistory(nil, ch, svc); len(got) != 0 {
		t.Errorf("no disclosure was served this turn, so none should be prepended; got %d", len(got))
	}
}
