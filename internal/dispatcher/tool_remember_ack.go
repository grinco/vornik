package dispatcher

import (
	"strings"

	"vornik.io/vornik/internal/conversation"
)

// SLICE 3, design §5.3.3 + §5.6 — the acknowledgement side of the shared-scope two-step.
//
// Two independent questions, deliberately separate functions:
//
//	isAcknowledgeableTurn — MAY this turn acknowledge anything? (§5.6)
//	isShareAcknowledgement — does this text acknowledge? (§5.3.3)
//
// The first is the security boundary and is checked FIRST, so a turn that may not acknowledge is
// never even matched against the phrase set.

// shareAcknowledgementPhrases is the closed set that acknowledges a shared-scope write.
//
// CLOSED AND MATCHED WHOLE, never with strings.Contains — see §5.6.3 and
// TestIsShareAcknowledgement: internal/slack/images.go:133 composes a file-upload caption as
// "(shared an image: " + name + ")" where `name` is the UPLOADED FILE'S NAME and therefore
// attacker-controlled, in a Text that already contains the word "shared". Under a substring
// match, a crafted upload would acknowledge a pending shared write with nothing typed.
//
// A bare "yes"/"ano" is deliberately absent (operator decision 2026-07-30): it may be answering
// a different question the model asked in the same turn, and on a confidentiality boundary that
// collision is not acceptable. The cost is that users must be TOLD the phrase, which is why the
// confirmation request lists it (§5.3.3).
//
// Czech appears with and without diacritics because people type Czech without them, and this
// deployment is mixed Czech/English (§3). An English-only set would strand half the users.
var shareAcknowledgementPhrases = map[string]bool{
	"share it":               true,
	"share it with the team": true,
	"confirm share":          true,
	"sdílej to":              true,
	"sdilej to":              true,
	"sdílet":                 true,
	"sdilet":                 true,
	"potvrzuji sdílení":      true,
	"potvrzuji sdileni":      true,
}

// isShareAcknowledgement reports whether the text is EXACTLY one of the accepted phrases.
//
// Normalisation is limited to what cannot change meaning: case, surrounding whitespace,
// collapsed internal whitespace, and trailing sentence punctuation. Anything else — including a
// phrase embedded in a longer sentence — is not an acknowledgement.
func isShareAcknowledgement(text string) bool {
	s := strings.ToLower(strings.Join(strings.Fields(text), " "))
	s = strings.TrimRight(s, ".!…")
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return shareAcknowledgementPhrases[s]
}

// voiceChannelSpecificPrefix marks a turn whose Text is an ASR transcript.
//
// Both voice channels set voice.inbound="true" UNCONDITIONALLY —
// internal/telegram/channel.go:324 and internal/slack/voice.go:304 (in the map literal, not a
// later conditional assignment) — so that key alone would be sufficient today. The check is by
// PREFIX anyway, as defence in depth: the remaining voice.* keys (duration_ms,
// transcript_confidence, language) are each conditional on being non-zero, and a future channel
// that surfaces transcript metadata without adopting the voice.inbound convention would still
// be caught.
//
// Matching a prefix rather than an exact key is the safe direction here: a new voice.* key
// causes a REFUSAL (typed acknowledgement required), never a silent acceptance.
const voiceChannelSpecificPrefix = "voice."

// isAcknowledgeableTurn reports whether this inbound turn is allowed to acknowledge a pending
// shared-scope write (design §5.6).
//
// TWO REFUSALS, both load-bearing:
//
//  1. NO SPEAKER — SpeakerID is empty. This is how the codebase already marks a synthetic turn
//     (channel_receiver.go:237-240 leaves operatorID empty for exactly these), and two paths
//     put MODEL-AUTHORED text in the user position: email/followup.go:157 and
//     telegram/bot.go:1344. Without this check a model could forge an acknowledgement by
//     arranging for its own text to arrive as a follow-up. See TestSyntheticTurnCannotAcknowledge.
//
//  2. VOICE — the Text is an ASR transcript. The human's own words, but through a probabilistic
//     channel, and no confidence threshold makes a 2-3 word phrase safe to infer from audio.
//
// FAILS CLOSED BY CONSTRUCTION: the zero value of SpeakerID is "", so a future synthetic path
// that forgets the sentinel entirely is refused rather than trusted. That asymmetry is why
// §5.6.4 declined a `Synthetic bool`, whose zero value (false) would mean "human" and would
// fail OPEN.
func isAcknowledgeableTurn(msg conversation.ChannelMessage) bool {
	if strings.TrimSpace(msg.SpeakerID) == "" {
		return false
	}
	for k := range msg.ChannelSpecific {
		if strings.HasPrefix(k, voiceChannelSpecificPrefix) {
			return false
		}
	}
	return true
}
