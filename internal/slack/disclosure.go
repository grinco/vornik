package slack

import (
	"strings"

	"vornik.io/vornik/internal/conversation"
)

// DisclosureScope implements conversation.DisclosureScoper: the identity the EU AI Act
// Art 50(1) notice is tracked against.
//
// OPERATOR REPORT 2026-07-30 — "the you are interacting with an AI system is sent
// multiple times: when the robot replies first time, and then when I ask a question in a
// thread." Slack's SessionID is `<team>/<channel>#<thread_ts>`, so using it as the
// disclosure identity made every new thread a fresh first interaction.
//
// Art 50(1) obliges the provider to ensure the natural persons CONCERNED are informed
// that they are interacting with an AI system, in a clear and distinguishable manner, at
// the latest at the time of the FIRST interaction. Nothing in it requires re-informing
// someone who has already been told, and repetition arguably works against the
// "clear and distinguishable" purpose by training people to ignore the banner. So the
// identity is the PERSON in the CHANNEL, which is stable across every thread in it.
//
// Per-person rather than per-channel, deliberately, and this is the more important half:
// keying on the channel alone would mark it served for whoever spoke first and deny the
// notice to every later participant. That was in fact the pre-existing behaviour for
// channel-level turns in a shared channel — the operator's three-person channel had one
// notice covering all three people — and it is an actual conformity gap rather than a
// UX blemish. Per-person closes it while still not repeating per thread.
//
// A DM is its own channel id, so a DM gets its own notice: someone opening a DM may
// never have read the one posted in the shared channel.
//
// Returns the session id unchanged when it cannot be parsed, or when there is no
// speaker. That degrades toward MORE disclosure (a per-thread key), never less — the
// same direction every other decision in internal/aidisclosure takes.
func (c *Channel) DisclosureScope(msg conversation.ChannelMessage) string {
	speaker := strings.TrimSpace(msg.SpeakerID)
	if speaker == "" {
		return msg.SessionID
	}
	teamID, channelID, _, err := parseSlackSessionID(msg.SessionID)
	if err != nil {
		return msg.SessionID
	}
	// "|" cannot appear in a Slack team, channel or user id, so the two halves of the
	// key can never be confused for one another.
	return teamID + "/" + channelID + "|" + speaker
}
