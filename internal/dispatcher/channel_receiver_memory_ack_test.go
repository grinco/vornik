package dispatcher

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// SLICE 3 (part 2) of the chat memory-write design §5.3.2 step 2 + §5.6 — the acknowledgement
// hook in ChannelReceiver.Receive. These tests drive Receive end to end (through the stub
// Channel + stub Agent) and assert on whether the pending confirmation ends up acknowledged.

// seedPending puts an unacknowledged confirmation into the repo for (channel, testMemSession,
// operator).
func seedPending(repo *fakeConfirmRepo, channel, operator string) {
	now := time.Now()
	_ = repo.Propose(context.Background(), &persistence.ChatMemoryWriteConfirmation{
		Channel: channel, SessionID: testMemSession, ContentFingerprint: "fp",
		Scope: string(memoryScopeShared), OperatorID: operator,
		ProposedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	})
}

func receiverWithConfirms(repo *fakeConfirmRepo) *ChannelReceiver {
	return &ChannelReceiver{
		Channel:                  &stubChannel{name: testMemChannel},
		Agent:                    &stubAgent{processResult: Result{Text: "ok"}},
		MemoryWriteConfirmations: repo,
	}
}

// The happy path: a human types an acknowledgement phrase as the ENTIRE message, with a real
// SpeakerID, while a pending confirmation waits. The receiver stamps it BEFORE the dispatcher
// runs, so the next remember() call can authorize the write.
func TestReceiver_HumanAcknowledgementStampsPendingConfirmation(t *testing.T) {
	repo := newFakeConfirmRepo(nil)
	seedPending(repo, "slack", "slack:UALICE")
	rcv := receiverWithConfirms(repo)

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UALICE", Text: "share it",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	row, ok := repo.get("slack")
	if !ok || !row.Acknowledged() {
		t.Fatalf("a human acknowledgement must stamp the pending row; ok=%v row=%+v", ok, row)
	}
}

// A SYNTHETIC turn (empty SpeakerID — the sentinel two model-authored paths carry:
// email/followup.go:157 and telegram/bot.go:1344) must NEVER acknowledge, even carrying the
// phrase as its entire text. The boundary is SpeakerID != "", which fails CLOSED on the zero
// value — a Synthetic bool was declined precisely because its false zero value means "human"
// and would fail OPEN (§5.6.4). A NEW SYNTHETIC PATH MUST BE ADDED TO THIS TEST.
func TestReceiver_SyntheticTurnDoesNotAcknowledge(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  conversation.ChannelMessage
	}{
		{
			name: "email auto-resume shape (email/followup.go:157, empty SpeakerID)",
			msg:  conversation.ChannelMessage{Source: "email", SessionID: "sess", Text: "share it"},
		},
		{
			name: "telegram triggerFollowup shape (telegram/bot.go:1344, UserID 0 -> empty SpeakerID)",
			msg:  conversation.ChannelMessage{Source: "telegram", SessionID: "sess", Text: "share it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeConfirmRepo(nil)
			// Seed against whatever operator id the synthetic path would produce — but note it
			// produces an empty operator id, so nothing could match anyway. Seed the plausible
			// human operator to prove the refusal is the SpeakerID gate, not a lookup miss.
			seedPending(repo, tc.msg.Source, tc.msg.Source+":someone")
			ch := &stubChannel{name: tc.msg.Source}
			rcv := &ChannelReceiver{Channel: ch, Agent: &stubAgent{processResult: Result{Text: "ok"}}, MemoryWriteConfirmations: repo}

			if err := rcv.Receive(context.Background(), tc.msg); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if row, _ := repo.get(tc.msg.Source); row.Acknowledged() {
				t.Error("a synthetic turn (no SpeakerID) must not acknowledge a shared write")
			}
		})
	}
}

// A VOICE turn carries the human's own words but through a probabilistic ASR channel, so it is
// not acknowledgeable — no confidence threshold makes a 2-3 word phrase safe to infer from audio
// (§5.6.2).
func TestReceiver_VoiceTurnDoesNotAcknowledge(t *testing.T) {
	repo := newFakeConfirmRepo(nil)
	seedPending(repo, "slack", "slack:UALICE")
	rcv := receiverWithConfirms(repo)

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UALICE", Text: "share it",
		ChannelSpecific: map[string]string{"voice.inbound": "true", "voice.transcript_confidence": "0.98"},
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if row, _ := repo.get("slack"); row.Acknowledged() {
		t.Error("a voice turn must not acknowledge a shared write")
	}
}

// WHOLE-MESSAGE, not strings.Contains (§5.6.3): a Slack file-upload caption is machine-composed
// as "(shared an image: <filename>)" under a real SpeakerID, and <filename> is
// attacker-controlled. A caption that embeds the phrase must NOT acknowledge — under a substring
// match it would forge an acknowledgement with nothing typed.
func TestReceiver_FileUploadCaptionDoesNotAcknowledge(t *testing.T) {
	repo := newFakeConfirmRepo(nil)
	seedPending(repo, "slack", "slack:UALICE")
	rcv := receiverWithConfirms(repo)

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UALICE",
		// The caption already contains "shared" and embeds the phrase as a filename.
		Text: "(shared an image: share it)",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if row, _ := repo.get("slack"); row.Acknowledged() {
		t.Error("a file-upload caption embedding the phrase must not acknowledge (whole-message rule)")
	}
}

// Bob cannot discharge Alice's proposal: the receiver derives the operator from the inbound
// turn, and the store's Acknowledge only stamps on an operator match (§5.3.3).
func TestReceiver_DifferentSpeakerDoesNotAcknowledge(t *testing.T) {
	repo := newFakeConfirmRepo(nil)
	seedPending(repo, "slack", "slack:UALICE")
	rcv := receiverWithConfirms(repo)

	err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UBOB", Text: "share it",
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if row, _ := repo.get("slack"); row.Acknowledged() {
		t.Error("a different speaker must not be able to acknowledge Alice's proposal")
	}
}

// No pending row, and a nil store, must both be safe no-ops — the hook must never break a turn.
func TestReceiver_AckHookIsSafeWithNoPendingRowAndNilStore(t *testing.T) {
	// Phrase typed, but nothing pending.
	repo := newFakeConfirmRepo(nil)
	rcv := receiverWithConfirms(repo)
	if err := rcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UALICE", Text: "share it",
	}); err != nil {
		t.Fatalf("Receive with no pending row: %v", err)
	}

	// Nil store.
	ch := &stubChannel{name: "slack"}
	nilRcv := &ChannelReceiver{Channel: ch, Agent: &stubAgent{processResult: Result{Text: "ok"}}}
	if err := nilRcv.Receive(context.Background(), conversation.ChannelMessage{
		Source: "slack", SessionID: "sess", SpeakerID: "UALICE", Text: "share it",
	}); err != nil {
		t.Fatalf("Receive with nil confirmation store: %v", err)
	}
}

// SetMemoryWriteConfirmations must write through to the tool executor — the same late-bind
// regression that shipped the chat bug-report tool dark (1234aea37). A setter that only touched
// the agent field would leave the shared-scope path permanently reporting "not built".
func TestSetMemoryWriteConfirmations_WritesThroughToToolExecutor(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	confirms := newFakeConfirmRepo(nil)
	audit := newFakeAuditRepo(nil)

	a.SetMemoryWriteConfirmations(confirms, audit)

	if a.toolExecutor.memoryConfirms == nil || a.toolExecutor.memoryAudit == nil {
		t.Fatal("tool executor still holds nil repos — the shared path would stay dark forever")
	}
	// Nil-safe on a nil agent and a nil executor.
	var nilAgent *Agent
	nilAgent.SetMemoryWriteConfirmations(confirms, audit)
	(&Agent{}).SetMemoryWriteConfirmations(confirms, audit)
}
