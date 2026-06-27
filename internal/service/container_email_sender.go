package service

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/registry"
)

// channelSendCloser is the narrow surface emailSenderAdapter needs
// out of an email channel. Defined as an interface so the unit
// tests can inject fakes without spinning up a real IMAP/SMTP
// pair. *email.Channel satisfies this naturally — Send is its
// public outbound API.
type channelSendCloser interface {
	Send(ctx context.Context, msg conversation.ChannelMessage) (string, error)
}

// emailSenderAdapter implements dispatcher.EmailSender over the
// service container's per-project email channels. The send_email
// tool calls SendEmail(projectID, req); the adapter looks up the
// channel pinned to that project, builds the ChannelMessage
// envelope that channel.Send's resolveOutboundEnvelope path
// consumes (ChannelSpecific[to]/[subject]/[in_reply_to]) and
// returns the channel-assigned Message-ID.
//
// The channels + projects slices are paired by index — channel[i]
// is the email transport for project[i]. The container builds
// them that way in container_http.go. Nil entries at either index
// are tolerated: the adapter skips them in lookups so a partial
// boot doesn't crash the send.
type emailSenderAdapter struct {
	channels []channelSendCloser
	projects []*registry.Project
}

// newEmailSender wraps the boot channel+project slices into a
// dispatcher.EmailSender. Returns nil when no channels are wired
// so the dispatcher's send_email tool reports "not configured"
// rather than silently swallowing calls — operators see the gap
// in the LLM's reply instead of in a debugger.
func newEmailSender(channels []channelSendCloser, projects []*registry.Project) dispatcher.EmailSender {
	if len(channels) == 0 {
		return nil
	}
	return &emailSenderAdapter{channels: channels, projects: projects}
}

// SendEmail implements dispatcher.EmailSender. Looks up the
// channel paired with projectID, builds a ChannelMessage with the
// envelope fields set, and forwards to Channel.Send. The Source
// is set to the email channel name so receivers downstream don't
// loop the outbound back through the inbound parser.
func (a *emailSenderAdapter) SendEmail(ctx context.Context, projectID string, req dispatcher.EmailSendRequest) (string, error) {
	if a == nil {
		return "", fmt.Errorf("email sender: not configured")
	}
	ch := a.lookup(projectID)
	if ch == nil {
		return "", fmt.Errorf("email sender: no channel configured for project %q", projectID)
	}
	cs := map[string]string{
		"to":      req.To,
		"subject": req.Subject,
	}
	if req.InReplyTo != "" {
		cs["in_reply_to"] = req.InReplyTo
	}
	msg := conversation.ChannelMessage{
		Source:          "email",
		Text:            req.Body,
		ChannelSpecific: cs,
		// SessionID intentionally empty for a fresh compose; the
		// channel falls through to ChannelSpecific[to] via
		// resolveOutboundEnvelope when SessionID has no matching
		// thread.
	}
	if req.InReplyTo != "" {
		msg.SessionID = req.InReplyTo
		msg.InReplyTo = req.InReplyTo
	}
	return ch.Send(ctx, msg)
}

// lookup finds the channel paired with projectID. Skips nil
// entries on either side so a partially-constructed boot doesn't
// match by accident.
func (a *emailSenderAdapter) lookup(projectID string) channelSendCloser {
	for i, p := range a.projects {
		if i >= len(a.channels) {
			return nil
		}
		if p == nil || a.channels[i] == nil {
			continue
		}
		if p.ID == projectID {
			return a.channels[i]
		}
	}
	return nil
}

// adaptChannels converts the []*email.Channel slice the container
// holds into the interface slice the adapter consumes. Centralised
// here so the container wiring site stays a one-liner.
func adaptChannels(channels []*email.Channel) []channelSendCloser {
	if len(channels) == 0 {
		return nil
	}
	out := make([]channelSendCloser, len(channels))
	for i, c := range channels {
		if c == nil {
			continue
		}
		out[i] = c
	}
	return out
}
