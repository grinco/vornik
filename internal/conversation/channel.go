// Package conversation defines the abstraction over inbound /
// outbound user-facing messaging channels. vornik's first channel
// implementation is Telegram (internal/telegram); the planned set
// also covers the web chat UI, GitHub App (issue/PR comments),
// Slack bot, and email. This package owns ONLY the interface and
// shared types; per-channel implementations live in their own
// packages and import this one.
//
// Goal: when a new inbound integration lands (e.g. GitHub App),
// the dispatcher / task layer don't change at all. The new
// integration writes a single Channel implementation that
// translates the upstream message shape into this package's
// ChannelMessage, and forwards user-facing outputs back via Send.
//
// 2026.6.0 — design doc:
// https://docs.vornik.io
package conversation

import (
	"context"
	"time"
)

// Channel is the bidirectional integration surface for one
// messaging platform. Implementations:
//   - own the upstream transport (HTTP poll, webhook, SSE, etc.)
//   - translate inbound payloads into ChannelMessage and call
//     Receiver.Receive
//   - translate outbound ChannelMessage into the upstream shape
//     when Send is called
//
// Lifecycle: Start once at daemon boot to begin ingesting from the
// platform; Stop on shutdown. Send is safe to call concurrently
// with Receive; implementations must serialise their own internal
// transport state as needed.
type Channel interface {
	// Name returns a short stable identifier for this channel
	// (e.g. "telegram", "web-chat", "github-app", "slack").
	// Surfaces in logs, metrics labels, and the
	// ChannelMessage.Source field so downstream consumers can
	// branch on origin without type-asserting.
	Name() string

	// Start begins inbound message ingestion. Implementations
	// take a Receiver and call Receiver.Receive once per inbound
	// message. Start blocks until ctx is cancelled OR an
	// unrecoverable transport error occurs; callers spawn it in
	// a goroutine.
	Start(ctx context.Context, recv Receiver) error

	// Stop signals the in-progress Start to exit. Implementations
	// flush pending sends and close transport handles. Idempotent.
	Stop() error

	// Send delivers an outbound message via the channel. The
	// implementation translates the generic ChannelMessage into
	// the platform's wire shape. Returns the platform's
	// per-message id (when known) so callers can correlate
	// future replies (e.g. Telegram's reply_to_message_id, Slack
	// thread_ts).
	Send(ctx context.Context, msg ChannelMessage) (sentID string, err error)

	// ListSessions returns a snapshot of currently-active
	// conversation sessions on this channel. Operators query
	// this via the UI to see "who's chatting with the bot right
	// now"; the autonomy panel uses it to size the answer
	// queue. Implementations may return an empty slice when the
	// platform doesn't expose session enumeration.
	ListSessions(ctx context.Context) ([]Session, error)

	// ResolveSpeaker maps a channel-specific speaker identifier
	// to a vornik user profile. Today's two speakers are
	// (a) the Telegram user_id (numeric) and (b) the chat UI's
	// API-key-derived caller. Future channels (Slack, GitHub
	// App) bring their own speaker ID shape. Returns
	// ErrSpeakerUnknown when the channel doesn't recognise the
	// identifier; callers map that to an auth-rejection path
	// rather than a 500.
	ResolveSpeaker(ctx context.Context, channelSpeakerID string) (Speaker, error)
}

// StreamingChannel is the optional interface streaming-capable
// channels (Telegram message edits, Slack chat.update, the web
// chat SSE surface) implement on top of Channel. The dispatcher
// type-asserts a Channel to StreamingChannel before starting a
// streaming reply; channels that don't implement it (GitHub App,
// email — atomic-comment platforms) fall back to a single
// end-of-turn Send. Decided 2026-05-17; see
// https://docs.vornik.io
type StreamingChannel interface {
	Channel

	// StreamingSend opens an incremental outbound for the given
	// session. Implementations typically send a placeholder
	// upstream and return a Stream the caller feeds token-by-token.
	// On any error during streaming, callers fall back to
	// Channel.Send with the accumulated text.
	StreamingSend(ctx context.Context, sessionID string) (Stream, error)
}

// Stream is the incremental outbound handle returned by
// StreamingChannel.StreamingSend. The dispatcher pushes tokens
// via Append as they arrive from the LLM; Close finalises the
// upstream message. Implementations coalesce edits internally
// to stay under the platform's edits-per-second quota (Telegram
// and Slack both rate-limit edits).
type Stream interface {
	// Append adds text to the in-flight outbound. The returned
	// error is terminal — after one Append error, no further
	// Append or Close call will succeed; the caller falls back
	// to a final Channel.Send with the accumulated text.
	Append(text string) error

	// Close finalises the outbound. Implementations flush any
	// coalesced pending edit and return the platform's
	// per-message id (the same value Channel.Send would have
	// returned for a one-shot send). Calling Close after a
	// terminal Append error returns the original error without
	// re-attempting.
	Close() (sentID string, err error)
}

// Receiver is the narrow contract Channel implementations call to
// hand inbound messages to the orchestration layer. vornik's main
// Receiver wraps the dispatcher; tests supply stubs. Receive is
// called serially per channel — implementations don't need to
// worry about concurrent in-bound delivery from one Channel.
type Receiver interface {
	Receive(ctx context.Context, msg ChannelMessage) error
}

// ChannelMessage is the platform-agnostic message envelope. The
// surface intentionally LOSES some platform-specific detail
// (Telegram has 30+ fields per Update; this struct has ~10) to
// keep the dispatcher's surface narrow. Implementations that need
// to round-trip platform-specific metadata (e.g. Telegram's
// MessageThreadID for forum routing) put it under
// ChannelSpecific so the dispatcher can ignore it but the same
// implementation's Send call can read it back.
type ChannelMessage struct {
	// Source is the Channel.Name() of the origin (or destination
	// on Send). Set automatically by the Channel; callers don't
	// fill it.
	Source string

	// ID is the channel-assigned message identifier. Empty on
	// Send paths where the channel hasn't yet acknowledged
	// delivery; populated on inbound Receive paths and on the
	// sentID return value of Send.
	ID string

	// SessionID identifies the conversation thread within the
	// channel. For Telegram this is the chat_id; for Slack the
	// channel-id#thread-ts; for GitHub App the (repo, issue_id)
	// pair. Implementations choose a stable string encoding.
	SessionID string

	// SpeakerID identifies the user within the channel. Pair with
	// ResolveSpeaker to get a vornik Speaker. Per-channel
	// namespace — two different Channels may emit the same raw
	// SpeakerID for different actual humans.
	SpeakerID string

	// Text is the message body. UTF-8. May be empty for
	// attachment-only messages.
	Text string

	// Attachments lists files associated with the message. Each
	// Channel implementation knows how to fetch attachment
	// payloads on demand; this struct only carries the pointer
	// + metadata. Empty when the message has no attachments.
	Attachments []Attachment

	// InReplyTo is the channel-message-id this message replies
	// to, when the platform's UI surfaces a reply gesture.
	// Empty for top-level messages. Telegram's swipe-to-reply,
	// Slack's thread reply, and GitHub's "reply to comment" all
	// land here.
	InReplyTo string

	// ThreadID is non-empty when the message lives in a
	// platform thread / topic that's distinct from the
	// SessionID. Telegram Forum Topics, Slack threads, and
	// GitHub PR threads all use this field. Empty for flat
	// conversations.
	ThreadID string

	// Timestamp is the channel-side send time. Tests fix this;
	// production lets the channel set it from the upstream
	// payload.
	Timestamp time.Time

	// ChannelSpecific holds platform-specific metadata that
	// doesn't fit the generic shape — e.g. Telegram's
	// MessageThreadID, Slack's team_id, GitHub's installation_id.
	// The dispatcher ignores this map; the originating Channel's
	// Send implementation reads it back to reconstruct the
	// upstream wire shape for replies.
	ChannelSpecific map[string]string
}

// Attachment is the generic file-attachment envelope. Channels
// vary in how they expose attachment payloads (Telegram file_id
// vs. Slack file URL vs. GitHub raw content URL); implementations
// hide the difference behind Fetch.
type Attachment struct {
	// Name is the operator-visible filename (e.g. "resume.pdf").
	Name string

	// MimeType is the declared content type (e.g.
	// "application/pdf"). Empty when the channel doesn't
	// advertise one.
	MimeType string

	// SizeBytes is the declared payload size. Zero when the
	// channel doesn't advertise size up front.
	SizeBytes int64

	// ChannelRef is the channel-private identifier
	// implementations use to fetch payload bytes. The
	// dispatcher MUST NOT inspect this string; pass it to the
	// originating Channel's FetchAttachment method when the
	// bytes are actually needed.
	ChannelRef string

	// ArtifactID is the persistence.Artifact row ID when the
	// channel persisted the attachment via the artifact
	// repository (today: email channel; future: telegram).
	// Empty when the channel didn't persist (no repo wired,
	// payload-fetch-on-demand channels). When populated the
	// dispatcher surfaces it in the user-visible message text
	// so the LLM can call read_artifact(id=...) to pull the
	// bytes — without this, attachments arrived but stayed
	// invisible to the model.
	ArtifactID string

	// Extraction summarises a document-extraction result when
	// the receiving channel auto-triggered the document
	// pipeline for this attachment. Nil when extraction was
	// skipped (no registered extractor for the MIME type, or
	// extraction failed). When populated the dispatcher's
	// enrichUserContent surfaces it to the LLM so the lead
	// knows the file already landed in project memory and
	// doesn't schedule a redundant "process this" task.
	Extraction *ExtractionSummary
}

// ExtractionSummary is the operator-visible result of a successful
// auto-extraction. Kept narrow on purpose — only the fields the
// dispatcher's LLM needs to decide "is this already in memory?"
// and to compose a natural reply ("got <book>, indexed 18 chapters").
type ExtractionSummary struct {
	// ExtractedDocumentID is the row id from extracted_documents.
	// Surfaced so the LLM can cross-reference via the document_*
	// tools (Phase 2 of the design doc) once those land.
	ExtractedDocumentID string

	// Title / Author come from the extractor's metadata. Empty
	// when the source format didn't carry them.
	Title  string
	Author string

	// SectionCount is the number of structural units the
	// extractor produced (chapters for EPUB, pages for PDF,
	// transcript segments for audio).
	SectionCount int

	// ChunksIngested is how many project_memory_chunks rows
	// landed in memory. Zero when memory ingest was disabled
	// or failed — the extraction itself still succeeded.
	ChunksIngested int
}

// Speaker is the channel-agnostic identity surface the
// dispatcher consumes. Each Channel maps its native speaker
// identifier into a Speaker via ResolveSpeaker.
type Speaker struct {
	// ID is a vornik-stable identifier for this person.
	// Implementations may use "channel:<channel-specific-id>"
	// during the migration window; a future identity-merge
	// surface can unify across channels.
	ID string

	// DisplayName is operator-friendly (Telegram first_name,
	// Slack real_name, GitHub login).
	DisplayName string

	// ChannelHandle is the channel-specific username (Telegram
	// @handle, GitHub login). Empty when the channel doesn't
	// expose one.
	ChannelHandle string
}

// Session represents one active conversation on the channel.
// Returned by Channel.ListSessions for operator-visible UIs.
type Session struct {
	// ID is the channel's SessionID (see ChannelMessage.SessionID).
	ID string

	// Title is operator-friendly (Telegram chat title, Slack
	// channel name, "PR #42" for GitHub). Empty when the
	// channel doesn't expose one.
	Title string

	// LastActivity is the timestamp of the most recent message
	// on either side. Used by operator UI to sort sessions
	// newest-first.
	LastActivity time.Time

	// ParticipantCount is how many distinct speakers have sent
	// at least one message in this session. 1 for DMs.
	ParticipantCount int
}

// ErrSpeakerUnknown is returned by ResolveSpeaker when the
// supplied identifier doesn't map to any known speaker on the
// channel. Callers treat it as an auth-rejection signal.
var ErrSpeakerUnknown = constError("conversation: speaker not known on this channel")

// constError lets us define package-level error sentinels
// without pulling errors.New into the package scope; matches
// the pattern used elsewhere in vornik (e.g.
// persistence.ErrAPIKeyNotFound).
type constError string

func (e constError) Error() string { return string(e) }
