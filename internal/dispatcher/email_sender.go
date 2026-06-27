package dispatcher

import "context"

// EmailSender is the narrow seam the send_email tool depends on.
// The service container builds an adapter over the project's
// email.Channel slice; nil EmailSender disables the tool and the
// dispatcher returns a "not configured" message so the LLM falls
// back to "here's the content instead of sending it for you."
//
// Slice 1 — fresh compose only. Inbound-reply is handled by the
// ChannelReceiver.sendReply path already; this seam is for the
// LLM-initiated outbound case (e.g. "send a summary to X").
//
// Project-scoping is the implementation's responsibility: the
// adapter consults its EmailChannels + EmailProjects pairing to
// route projectID to the right channel, and rejects unknown IDs
// with a typed error. The tool layer doesn't see the channel
// directly — it only knows about projectID.
//
// Recipient gating: per slice-1 policy, the adapter checks the
// project's Email.SenderAllowlist (currently the inbound gate)
// against the To address. Same trust model both ways — the
// closed-loop assistant pattern. Slice 2 can split this with
// a recipient_allowlist if anyone needs asymmetric scoping.
type EmailSender interface {
	SendEmail(ctx context.Context, projectID string, req EmailSendRequest) (messageID string, err error)
}

// EmailSendRequest is the per-call parameter bundle. Plain text
// only in slice 1; HTML alternative bodies + Cc/Bcc land in slice
// 2 when there's a real need. Threading headers (InReplyTo) are
// optional — without one the channel composes a fresh message.
type EmailSendRequest struct {
	// To is the recipient address (RFC 5322 mailbox form). The
	// channel layer validates and rejects malformed values; the
	// tool only enforces non-empty.
	To string

	// Subject is the Subject: header. Required.
	Subject string

	// Body is the plain-text body. Newlines are preserved
	// verbatim; the channel normalises CRLF for the SMTP DATA
	// stream.
	Body string

	// InReplyTo, when non-empty, threads the outbound onto the
	// referenced inbound Message-ID (no angle-brackets — the
	// channel adds them). Optional: empty means "start a fresh
	// thread."
	InReplyTo string
}
