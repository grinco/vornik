// Package webchat implements the conversation.Channel for the
// web UI's per-project chat surface. Each browser session is a
// Channel session; the dispatcher's tool-calling loop runs
// synchronously per HTTP request and the final assistant reply
// is captured in the Channel's Sent buffer for the UI handler
// to render alongside the user's prompt.
//
// Design notes — see also https://docs.vornik.io
// channel-design.md, "Slice 4G — web chat":
//
//   - The Channel does NOT poll any upstream; it's a strict
//     outbound delegate. The UI handler builds a fresh Channel
//     per request, dispatches one turn via dispatcher.ChannelReceiver,
//     then reads back the captured reply.
//   - Sessions are keyed on a UUID cookie (set on the first GET).
//     History is held in memory in the package-level SessionStore
//     and survives across requests for the same browser. Daemon
//     restart clears history — the operator can re-prompt; we
//     don't try to persist short-lived chat in the database.
//   - Streaming is not wired today: the UI handler completes one
//     full dispatcher turn before rendering. Adding SSE later is
//     additive — implement conversation.StreamingChannel here and
//     the receiver picks it up via the existing type-assertion.
package webchat

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/conversation"
)

// ChannelName is the conversation.Channel.Name value the receiver
// stamps on every inbound ChannelMessage from this package. Exported
// so tests across the codebase can match against the constant rather
// than hardcoding the string in three places.
const ChannelName = "web-chat"

// Channel is the per-request synchronous outbound surface. A new
// Channel is constructed each turn; Send appends the final assistant
// text to Sent. Tests + the UI handler read Sent back to render the
// reply. Start / Stop / ListSessions / ResolveSpeaker are no-ops
// because the channel has no long-lived upstream; the only inbound
// path is the UI's POST handler, which calls Receiver.Receive
// directly.
type Channel struct {
	// ProjectID is the per-channel project pin — every session on
	// this channel is scoped to a single project (chosen by the
	// browser URL: /ui/projects/<projectID>/chat). The SessionStore
	// also bakes this in for its dispatcher.Session.ActiveProject.
	ProjectID string

	// Speaker is the resolved vornik identity for the browser
	// session. Populated by the UI handler from the request's API
	// key when known; an unauthenticated chat surface fills in a
	// synthetic display name.
	Speaker conversation.Speaker

	mu   sync.Mutex
	sent []string
}

// New returns a fresh Channel ready to capture one turn's reply.
// The UI handler builds one per request — never reuse across
// requests, the Sent buffer would leak prior responses into the
// next render.
func New(projectID string, speaker conversation.Speaker) *Channel {
	return &Channel{ProjectID: projectID, Speaker: speaker}
}

// Name implements conversation.Channel.
func (c *Channel) Name() string { return ChannelName }

// Start is a no-op for the web chat. The Channel is not a long-lived
// puller; each HTTP request constructs a fresh one and hands it to
// the receiver synchronously. Returning nil rather than an error
// keeps callers that lifecycle every Channel uniformly happy.
func (c *Channel) Start(_ context.Context, _ conversation.Receiver) error {
	return nil
}

// Stop is a no-op for the same reason as Start.
func (c *Channel) Stop() error { return nil }

// Send captures the dispatcher's outbound assistant text. Returns
// a synthetic ID (the buffer index as a decimal string) so callers
// that correlate via the returned id don't see an empty string.
// Empty text is dropped — the UI handler renders the captured
// buffer; an empty Send call would render as a blank message.
func (c *Channel) Send(_ context.Context, msg conversation.ChannelMessage) (string, error) {
	if msg.Text == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg.Text)
	return webChatSentID(len(c.sent) - 1), nil
}

// Sent returns the captured outbound messages for this turn,
// in send order. The UI handler joins them with a blank line to
// render — matching how dispatcher.Result.Text is wrapped by the
// ResultPostprocessor before reaching the channel.
func (c *Channel) Sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.sent))
	copy(out, c.sent)
	return out
}

// ListSessions returns nil — the web chat doesn't expose a session
// listing surface today. Operators inspect chat sessions via the UI
// directly; the autonomy panel doesn't size off web-chat depth.
func (c *Channel) ListSessions(_ context.Context) ([]conversation.Session, error) {
	return nil, nil
}

// ResolveSpeaker echoes the Speaker the Channel was constructed
// with — speaker auth happens at the HTTP middleware layer; by the
// time the Channel exists the caller has already passed an API
// key check. Unknown speakers map to a synthetic "anonymous"
// identity rather than returning ErrSpeakerUnknown so the
// dispatcher still runs (the project ACL surface is the
// authoritative permission check, not channel speaker resolution).
func (c *Channel) ResolveSpeaker(_ context.Context, channelSpeakerID string) (conversation.Speaker, error) {
	if c.Speaker.ID != "" {
		return c.Speaker, nil
	}
	return conversation.Speaker{
		ID:          "web-chat:" + channelSpeakerID,
		DisplayName: "Web chat user",
	}, nil
}

// webChatSentID renders a stable, log-friendly id for the
// captured outbound — "<index>". Distinct from int.String to make
// future ID-format changes a one-line edit.
func webChatSentID(index int) string {
	if index < 0 {
		return ""
	}
	// Small lookup avoids an strconv import that the rest of this
	// file doesn't need.
	const digits = "0123456789"
	if index < 10 {
		return string(digits[index])
	}
	var b []byte
	for n := index; n > 0; n /= 10 {
		b = append([]byte{digits[n%10]}, b...)
	}
	return string(b)
}

// Compile-time guarantee.
var _ conversation.Channel = (*Channel)(nil)
