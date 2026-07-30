package slack

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// OPERATOR REPORT 2026-07-30: the "you are interacting with an AI system" notice arrived
// again every time a question was asked in a thread.
//
// Slack's SessionID is `<team>/<channel>#<thread_ts>`, so each thread read as a fresh
// first interaction. Art 50(1) is owed to a PERSON at their first interaction, so the
// disclosure identity is the person and the channel — stable across every thread in it.
func TestDisclosureScope_StableAcrossThreadsInTheSameChannel(t *testing.T) {
	ch := makeChannel(t, validConfig(), time.Unix(1700000000, 0))

	scope := func(sessionID, speaker string) string {
		return ch.DisclosureScope(conversation.ChannelMessage{
			SessionID: sessionID,
			SpeakerID: speaker,
		})
	}

	channelLevel := scope("T123/C_general#main", "U_alice")
	firstThread := scope("T123/C_general#1785367141.211839", "U_alice")
	secondThread := scope("T123/C_general#1785999999.000100", "U_alice")

	if channelLevel != firstThread || firstThread != secondThread {
		t.Fatalf("scopes differ across threads: %q / %q / %q — the same person in the "+
			"same channel is one first interaction", channelLevel, firstThread, secondThread)
	}
	if channelLevel == "" {
		t.Fatal("scope is empty, which would silently fall back to the session id")
	}
}

// Each natural person is owed the notice, so the scope must separate them. Getting this
// wrong in the other direction is worse than a repeat: before this change, in a shared
// channel the channel-scoped session was marked served by whoever spoke first, so every
// later participant received NO notice at all.
func TestDisclosureScope_SeparatesPeopleAndChannels(t *testing.T) {
	ch := makeChannel(t, validConfig(), time.Unix(1700000000, 0))

	scope := func(sessionID, speaker string) string {
		return ch.DisclosureScope(conversation.ChannelMessage{SessionID: sessionID, SpeakerID: speaker})
	}

	alice := scope("T123/C_general#main", "U_alice")
	bob := scope("T123/C_general#main", "U_bob")
	if alice == bob {
		t.Error("two people share a disclosure scope; each is owed the notice at their " +
			"own first interaction")
	}

	otherChannel := scope("T123/C_other#main", "U_alice")
	if alice == otherChannel {
		t.Error("the same person in a different channel shares a scope; the notice has " +
			"to appear in each place they are reading")
	}

	otherTeam := scope("T999/C_general#main", "U_alice")
	if alice == otherTeam {
		t.Error("two workspaces share a disclosure scope")
	}
}

// A DM is its own channel, so it gets its own notice — correct, since the person opening
// a DM may never have read the one posted in a shared channel.
func TestDisclosureScope_DMIsItsOwnScope(t *testing.T) {
	ch := makeChannel(t, validConfig(), time.Unix(1700000000, 0))
	inChannel := ch.DisclosureScope(conversation.ChannelMessage{
		SessionID: "T123/C_general#main", SpeakerID: "U_alice"})
	inDM := ch.DisclosureScope(conversation.ChannelMessage{
		SessionID: "T123/D0BLA9ZRDFH#main", SpeakerID: "U_alice"})
	if inChannel == inDM {
		t.Error("a DM shares a scope with the shared channel")
	}
}

// Degenerate input must not collapse distinct scopes into one shared key, which would
// suppress the notice for everyone after the first. An unparseable session falls back to
// the session id itself.
func TestDisclosureScope_DegenerateInputDoesNotCollapse(t *testing.T) {
	ch := makeChannel(t, validConfig(), time.Unix(1700000000, 0))

	noSpeaker := ch.DisclosureScope(conversation.ChannelMessage{SessionID: "T123/C_general#main"})
	if noSpeaker != "" && noSpeaker == ch.DisclosureScope(conversation.ChannelMessage{
		SessionID: "T123/C_general#main", SpeakerID: "U_alice"}) {
		t.Error("a message with no speaker shares Alice's scope")
	}

	unparseable := ch.DisclosureScope(conversation.ChannelMessage{
		SessionID: "not-a-slack-session", SpeakerID: "U_alice"})
	if unparseable == "" {
		t.Error("an unparseable session produced an empty scope; the receiver would " +
			"then fall back to the session id, which is the safe answer, but returning " +
			"it explicitly is clearer")
	}
}

// Compile-time proof the channel satisfies the interface the receiver type-asserts on.
// Without it, a signature drift would silently restore per-thread disclosure.
func TestChannel_SatisfiesDisclosureScoper(_ *testing.T) {
	var _ conversation.DisclosureScoper = (*Channel)(nil)
}
