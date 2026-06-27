package dispatcher

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// TestChannelReceiver_PopulatesOperatorID: ChannelReceiver.Receive
// must stamp Request.OperatorID with "<channel-source>:<speaker_id>"
// so the dispatcher's per-turn operator-profile lookup has a
// stable key. Without this, the profile-read path in
// Agent.Process gets an empty OperatorID and silently skips the
// injection — operators get default-verbose replies on every
// channel regardless of accumulated preferences.
func TestChannelReceiver_PopulatesOperatorID(t *testing.T) {
	ch := &stubChannel{name: "telegram"}
	agent := &stubAgent{processResult: Result{Text: "ok"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source:    "telegram",
		SessionID: "chat-1",
		SpeakerID: "42",
		Text:      "hi",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if agent.lastReq.OperatorID != "telegram:42" {
		t.Errorf("OperatorID = %q, want telegram:42", agent.lastReq.OperatorID)
	}
}

// TestChannelReceiver_OperatorIDEmptyWhenSpeakerMissing: a
// channel that doesn't supply SpeakerID (synthetic / system
// turns) yields an empty OperatorID so the dispatcher skips
// the profile lookup. Better than building a malformed
// "telegram:" key that would collide across users.
func TestChannelReceiver_OperatorIDEmptyWhenSpeakerMissing(t *testing.T) {
	ch := &stubChannel{name: "telegram"}
	agent := &stubAgent{processResult: Result{Text: "ok"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	_ = rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source:    "telegram",
		SessionID: "chat-1",
		// SpeakerID intentionally absent
		Text: "hi",
	})
	if agent.lastReq.OperatorID != "" {
		t.Errorf("OperatorID = %q, want empty when SpeakerID missing", agent.lastReq.OperatorID)
	}
}

// TestChannelReceiver_OperatorIDUsesMessageSource: when
// ChannelMessage.Source differs from Channel.Name() (rare —
// fan-in scenarios), the OperatorID uses the message's Source
// so a profile attached to "slack:U123" stays scoped to slack
// even if a future re-route puts it through a different
// receiver. Belt-and-suspenders against confusion.
func TestChannelReceiver_OperatorIDUsesMessageSource(t *testing.T) {
	ch := &stubChannel{name: "different-channel-name"}
	agent := &stubAgent{processResult: Result{Text: "ok"}}
	rcv := &ChannelReceiver{Channel: ch, Agent: agent}

	_ = rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source:    "slack",
		SessionID: "T1/C2#1234.567",
		SpeakerID: "U123",
		Text:      "hi",
	})
	if agent.lastReq.OperatorID != "slack:U123" {
		t.Errorf("OperatorID = %q, want slack:U123", agent.lastReq.OperatorID)
	}
}
