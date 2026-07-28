package aidisclosure

// Cadence is how often a channel must carry the AI-interaction disclosure.
type Cadence int

const (
	// CadencePerSession serves the notice once, as a distinct message,
	// before the first assistant reply in a session. For continuous
	// conversational UIs where the user sees an unbroken thread and the
	// disclosure stays visible in the scrollback.
	CadencePerSession Cadence = iota

	// CadencePerMessage appends the notice to EVERY outbound. For channels
	// whose messages are standalone artifacts: an email reply gets
	// forwarded, a GitHub comment gets linked and quoted, and either can
	// reach a reader who never saw the start of the thread.
	CadencePerMessage
)

func (c Cadence) String() string {
	switch c {
	case CadencePerSession:
		return "per-session"
	case CadencePerMessage:
		return "per-message"
	default:
		return "unknown"
	}
}

// perSessionChannels lists the conversation.Channel Name() values whose
// surface is a continuous conversation. Everything NOT in this set — which
// includes any channel added after this was written — gets per-message.
//
// The default direction is deliberate and is pinned by
// TestCadenceFor_UnknownChannel_DefaultsToPerMessage: a future channel that
// nobody remembers to classify will over-disclose. Over-disclosure is a UX
// complaint; under-disclosure is an EU AI Act Art 50(1) non-conformity
// carrying up to EUR 15M or 3% of worldwide turnover.
var perSessionChannels = map[string]struct{}{
	"telegram": {},
	"slack":    {},
	"web-chat": {},
}

// CadenceFor maps a conversation.Channel Name() to its disclosure cadence.
func CadenceFor(channel string) Cadence {
	if _, ok := perSessionChannels[channel]; ok {
		return CadencePerSession
	}
	return CadencePerMessage
}
